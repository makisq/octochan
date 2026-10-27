package lang

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"octochan/config"
)

// AliasExtension — расширение файлов с алиасами.
const AliasExtension = ".ochan"

// ManifestName — описание пакета алиасов: версия и назначение. Лежит
// в каталоге пакета рядом с его файлами .ochan.
const ManifestName = "package.yaml"

// versionPattern — версия из одного, двух или трёх чисел: 2, 2.3, 2.3.1.
var versionPattern = regexp.MustCompile(`^v?(\d+(?:\.\d+){0,2})$`)

// priorPattern — условный счёт правила: «успехи/применения», например 8/8.
var priorPattern = regexp.MustCompile(`^(\d+)/(\d+)$`)

// Prior — условный счёт автора скилла для алиаса: «как будто алиас
// применялся Uses раз и Successes из них прошли» (раздел 7.5 манифеста ФПм).
// Без него у нового скилла нет журнала, и каждое его правило проходило бы
// кольцо 2 только после долгой проверки.
type Prior struct {
	Successes int
	Uses      int
}

// ParsePrior разбирает запись «успехи/применения».
func ParsePrior(text string) (Prior, error) {
	m := priorPattern.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return Prior{}, fmt.Errorf("условный счёт %q, ожидалось «успехи/применения», например 8/8", text)
	}
	var p Prior
	fmt.Sscan(m[1], &p.Successes)
	fmt.Sscan(m[2], &p.Uses)
	if p.Successes > p.Uses {
		return Prior{}, fmt.Errorf("условный счёт %q: успехов больше, чем применений", text)
	}
	return p, nil
}

// Package — описание пакета алиасов из его манифеста.
type Package struct {
	Name        string
	Version     string
	Description string
	// Priors — условный счёт автора для алиасов пакета: ключи манифеста
	// prior.<алиас>.
	Priors map[string]Prior
	// Manifest — путь к файлу описания.
	Manifest string
}

// LoadManifest читает описание пакета. Пакет без манифеста — не ошибка:
// он просто без версии, и nil это и означает.
//
// Манифест разбирается тем же разборщиком, что и конфиги: строки
// «ключ: значение». Неизвестный ключ — ошибка: опечатка в «version»
// иначе тихо сделала бы пакет безверсионным.
func LoadManifest(name, dir string) (*Package, error) {
	path := filepath.Join(dir, ManifestName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать %s: %w", path, err)
	}

	fields, err := config.ParseConfig(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	pkg := &Package{Name: name, Manifest: path}
	for section, params := range fields {
		if section != "" {
			return nil, fmt.Errorf("%s: секции не поддерживаются: [%s]", path, section)
		}
		for key, value := range params {
			switch key {
			case "name":
				if value != name {
					return nil, fmt.Errorf("%s: пакет называется %q, а лежит в каталоге %q — имя пакета задаёт каталог",
						path, value, name)
				}
			case "version":
				m := versionPattern.FindStringSubmatch(value)
				if m == nil {
					return nil, fmt.Errorf("%s: версия %q, ожидалось вида 1, 1.2 или 1.2.3", path, value)
				}
				pkg.Version = m[1]
			case "description":
				pkg.Description = value
			default:
				alias, ok := strings.CutPrefix(key, "prior.")
				if !ok || alias == "" {
					return nil, fmt.Errorf("%s: неизвестное поле %q; допустимы name, version, description и prior.<алиас>",
						path, key)
				}
				prior, err := ParsePrior(value)
				if err != nil {
					return nil, fmt.Errorf("%s: %s: %w", path, key, err)
				}
				if pkg.Priors == nil {
					pkg.Priors = map[string]Prior{}
				}
				pkg.Priors[alias] = prior
			}
		}
	}
	if pkg.Version == "" {
		return nil, fmt.Errorf("%s: не указана version", path)
	}
	return pkg, nil
}

// Registry хранит алиасы, разложенные по пространствам имён.
//
// Пространства нужны потому, что модуль — это не только Go-плагин, но и пакет
// алиасов: если кто-то опубликовал набор из сотни devops-алиасов, его имена
// не должны молча перекрывать пользовательские. Алиасы пользователя живут
// в пустом пространстве и доступны без префикса, алиасы пакета — как
// пакет.алиас, а также без префикса, пока имя не занято.
type Registry struct {
	// modules[пространство][имя] = объявление
	modules map[string]map[string]*AliasDecl
	// types[пространство][имя] — записи (*RecordDecl) и интерфейсы
	// (*InterfaceDecl).
	types map[string]map[string]any
	// order хранит порядок подключения пакетов для разрешения коротких имён
	order []string
	// packages — описания пакетов, у которых есть манифест.
	packages map[string]*Package
	// renamed — вторые имена пакетов из import пакет as имя.
	renamed map[string]string
}

// NewRegistry создаёт пустой реестр.
func NewRegistry() *Registry {
	return &Registry{
		modules:  map[string]map[string]*AliasDecl{"": {}},
		types:    map[string]map[string]any{"": {}},
		packages: map[string]*Package{},
		renamed:  map[string]string{},
	}
}

// resolve переводит второе имя пакета в настоящее.
func (r *Registry) resolve(module string) string {
	if real, ok := r.renamed[module]; ok {
		return real
	}
	return module
}

// AddFile регистрирует всё, что объявлено в файле: импорты, записи,
// интерфейсы и алиасы.
func (r *Registry) AddFile(module string, file *File) error {
	for _, imp := range file.Imports {
		if err := r.Import(imp); err != nil {
			return err
		}
	}
	for _, decl := range file.Records {
		decl.Module = module
		if err := r.addType(module, decl.Name, decl, decl.Pos); err != nil {
			return err
		}
	}
	for _, decl := range file.Interfaces {
		decl.Module = module
		if err := r.addType(module, decl.Name, decl, decl.Pos); err != nil {
			return err
		}
	}
	for _, decl := range file.Aliases {
		if err := r.Add(module, decl); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) addType(module, name string, decl any, pos Position) error {
	space, ok := r.types[module]
	if !ok {
		space = map[string]any{}
		r.types[module] = space
		if _, known := r.modules[module]; !known && module != "" {
			r.modules[module] = map[string]*AliasDecl{}
			r.order = append(r.order, module)
		}
	}
	if _, err := ParseTypeName(name); err == nil {
		return fmt.Errorf("%s: имя %s занято встроенным типом", pos, name)
	}
	if existing, clash := space[name]; clash {
		return fmt.Errorf("%s: тип %q уже объявлен: %s", pos, name, typePos(existing))
	}
	if alias, clash := r.modules[module][name]; clash {
		return fmt.Errorf("%s: имя %q уже занято алиасом: %s", pos, name, alias.Pos)
	}
	space[name] = decl
	return nil
}

func typePos(decl any) Position {
	switch d := decl.(type) {
	case *RecordDecl:
		return d.Pos
	case *InterfaceDecl:
		return d.Pos
	}
	return Position{}
}

// LookupType ищет запись или интерфейс: с пакетом или по короткому имени,
// как алиас.
func (r *Registry) LookupType(module, name string) (any, error) {
	if module != "" {
		real := r.resolve(module)
		space, ok := r.types[real]
		if !ok {
			return nil, fmt.Errorf("пакет %q не подключён%s", module, r.suggestModule(module))
		}
		decl, ok := space[name]
		if !ok {
			return nil, fmt.Errorf("в пакете %s нет типа %q", module, name)
		}
		return decl, nil
	}
	if decl, ok := r.types[""][name]; ok {
		return decl, nil
	}
	var found any
	var sources []string
	for _, pkg := range r.order {
		if decl, ok := r.types[pkg][name]; ok {
			found = decl
			sources = append(sources, pkg)
		}
	}
	switch len(sources) {
	case 0:
		return nil, fmt.Errorf("неизвестный тип %q; встроенные: %s", name, strings.Join(knownTypeNames(), ", "))
	case 1:
		return found, nil
	default:
		return nil, fmt.Errorf("тип %q есть сразу в пакетах %s; уточните пакет, например %s.%s",
			name, strings.Join(sources, ", "), sources[0], name)
	}
}

// lookupTypeName ищет тип по имени из аннотации: «Тип» или «пакет.Тип».
func (r *Registry) lookupTypeName(t TypeName) (any, error) {
	module, name, qualified := strings.Cut(string(t), ".")
	if !qualified {
		module, name = "", string(t)
	}
	return r.LookupType(module, name)
}

// Import проверяет import: пакет подключён и подходит по версии; as
// даёт ему второе имя.
func (r *Registry) Import(imp *ImportDecl) error {
	if _, ok := r.modules[imp.Package]; !ok || imp.Package == "" {
		return fmt.Errorf("%s: import %s: пакет не подключён%s", imp.Pos, imp.Package, r.suggestModule(imp.Package))
	}
	if imp.Version != "" {
		pkg, ok := r.packages[imp.Package]
		if !ok {
			return fmt.Errorf("%s: import %s v%s: у пакета нет версии — нет %s", imp.Pos, imp.Package, imp.Version, ManifestName)
		}
		if !versionMatches(pkg.Version, imp.Version) {
			return fmt.Errorf("%s: import %s v%s: подключена версия %s", imp.Pos, imp.Package, imp.Version, pkg.Version)
		}
	}
	if imp.As != "" && imp.As != imp.Package {
		if _, taken := r.modules[imp.As]; taken {
			return fmt.Errorf("%s: import %s as %s: имя %s уже занято пакетом", imp.Pos, imp.Package, imp.As, imp.As)
		}
		if real, taken := r.renamed[imp.As]; taken && real != imp.Package {
			return fmt.Errorf("%s: import %s as %s: имя %s уже дано пакету %s", imp.Pos, imp.Package, imp.As, imp.As, real)
		}
		r.renamed[imp.As] = imp.Package
	}
	return nil
}

// versionMatches — версия подходит под требование: 2 подходит к 2.x.y,
// 2.1 — к 2.1.y, 2.1.3 — только к 2.1.3.
func versionMatches(have, want string) bool {
	h := strings.Split(have, ".")
	w := strings.Split(want, ".")
	if len(w) > len(h) {
		// 2.1 против требования 2.1.0: недостающие части — нули.
		for len(h) < len(w) {
			h = append(h, "0")
		}
	}
	for i := range w {
		if strings.TrimLeft(h[i], "0") != strings.TrimLeft(w[i], "0") {
			return false
		}
	}
	return true
}

// Package возвращает описание пакета, если у него есть манифест.
func (r *Registry) Package(module string) (*Package, bool) {
	pkg, ok := r.packages[r.resolve(module)]
	return pkg, ok
}

// Clone возвращает копию реестра. Объявления общие — их никто не
// меняет, — а пространства свои: алиасы, добавленные в копию, не видны
// в оригинале.
func (r *Registry) Clone() *Registry {
	clone := &Registry{
		modules:  make(map[string]map[string]*AliasDecl, len(r.modules)),
		types:    make(map[string]map[string]any, len(r.types)),
		order:    append([]string(nil), r.order...),
		packages: make(map[string]*Package, len(r.packages)),
		renamed:  make(map[string]string, len(r.renamed)),
	}
	for as, real := range r.renamed {
		clone.renamed[as] = real
	}
	for module, space := range r.types {
		copied := make(map[string]any, len(space))
		for name, decl := range space {
			copied[name] = decl
		}
		clone.types[module] = copied
	}
	for module, pkg := range r.packages {
		clone.packages[module] = pkg
	}
	for module, space := range r.modules {
		copied := make(map[string]*AliasDecl, len(space))
		for name, decl := range space {
			copied[name] = decl
		}
		clone.modules[module] = copied
	}
	return clone
}

// Add регистрирует алиас в пространстве module.
func (r *Registry) Add(module string, decl *AliasDecl) error {
	space, ok := r.modules[module]
	if !ok {
		space = make(map[string]*AliasDecl)
		r.modules[module] = space
		if module != "" {
			r.order = append(r.order, module)
		}
	}

	if existing, clash := space[decl.Name]; clash {
		where := "в пользовательских алиасах"
		if module != "" {
			where = "в пакете " + module
		}
		return fmt.Errorf("алиас %q объявлен дважды %s: %s и %s",
			decl.Name, where, existing.Pos, decl.Pos)
	}
	if existing, clash := r.types[module][decl.Name]; clash {
		return fmt.Errorf("%s: имя %q уже занято типом: %s", decl.Pos, decl.Name, typePos(existing))
	}

	decl.Module = module
	space[decl.Name] = decl
	return nil
}

// Lookup ищет алиас. Пустой module означает поиск без префикса:
// сначала среди пользовательских, затем среди пакетов.
func (r *Registry) Lookup(module, name string) (*AliasDecl, error) {
	if module != "" {
		module = r.resolve(module)
		space, ok := r.modules[module]
		if !ok {
			// Частая ошибка: имя файла написано без кавычек, и разбор
			// принимает prod.conf за обращение к пакету prod.
			if looksLikeFileName(name) {
				return nil, fmt.Errorf("%s.%s выглядит как имя файла; строки пишутся в кавычках: \"%s.%s\"",
					module, name, module, name)
			}
			return nil, fmt.Errorf("пакет алиасов %q не подключён%s", module, r.suggestModule(module))
		}
		decl, ok := space[name]
		if !ok {
			return nil, fmt.Errorf("в пакете %s нет алиаса %q", module, name)
		}
		return decl, nil
	}

	if decl, ok := r.modules[""][name]; ok {
		return decl, nil
	}

	// Короткое имя ищем по пакетам; неоднозначность — ошибка, а не
	// случайный выбор одного из вариантов.
	var found *AliasDecl
	var sources []string
	for _, pkg := range r.order {
		if decl, ok := r.modules[pkg][name]; ok {
			found = decl
			sources = append(sources, pkg)
		}
	}

	switch len(sources) {
	case 0:
		return nil, fmt.Errorf("алиас %q не найден", name)
	case 1:
		return found, nil
	default:
		return nil, fmt.Errorf("алиас %q есть сразу в пакетах %s; уточните пакет, например %s.%s",
			name, strings.Join(sources, ", "), sources[0], name)
	}
}

// fileExtensions — то, что чаще всего оказывается справа от точки,
// когда пользователь забыл кавычки вокруг пути.
var fileExtensions = map[string]bool{
	"conf": true, "cfg": true, "ini": true, "yaml": true, "yml": true,
	"json": true, "txt": true, "log": true, "ochan": true, "bin": true,
}

func looksLikeFileName(name string) bool { return fileExtensions[strings.ToLower(name)] }

func (r *Registry) suggestModule(module string) string {
	if len(r.order) == 0 {
		return ""
	}
	return fmt.Sprintf("; подключены: %s", strings.Join(r.order, ", "))
}

// Names возвращает все имена алиасов, доступные без префикса.
func (r *Registry) Names() []string {
	seen := make(map[string]bool)
	var names []string

	for name := range r.modules[""] {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, pkg := range r.order {
		for name := range r.modules[pkg] {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}

	sort.Strings(names)
	return names
}

// declared сообщает, что имя объявлено хотя бы в одном пакете. Нужен,
// чтобы отличить «алиаса нет» от «алиас есть в нескольких пакетах сразу».
func (r *Registry) declared(name string) bool {
	for _, pkg := range r.order {
		if _, ok := r.modules[pkg][name]; ok {
			return true
		}
	}
	return false
}

// Decls возвращает объявления всех алиасов, отсортированные по пакету и имени.
func (r *Registry) Decls() []*AliasDecl {
	var decls []*AliasDecl
	for _, space := range r.modules {
		for _, decl := range space {
			decls = append(decls, decl)
		}
	}
	sort.Slice(decls, func(i, j int) bool {
		if decls[i].Module != decls[j].Module {
			return decls[i].Module < decls[j].Module
		}
		return decls[i].Name < decls[j].Name
	})
	return decls
}

// AliasInfo — сведения об алиасе для команды списка.
type AliasInfo struct {
	Module string
	Name   string
	Params []string
	Doc    string
	Source string
}

// Signature возвращает сигнатуру вида пакет.имя(a, b).
func (a AliasInfo) Signature() string {
	name := a.Name
	if a.Module != "" {
		name = a.Module + "." + a.Name
	}
	return fmt.Sprintf("%s(%s)", name, strings.Join(a.Params, ", "))
}

// List перечисляет все алиасы, отсортированные по пакету и имени.
func (r *Registry) List() []AliasInfo {
	var infos []AliasInfo
	for module, space := range r.modules {
		for _, decl := range space {
			infos = append(infos, AliasInfo{
				Module: module,
				Name:   decl.Name,
				Params: decl.Params,
				Doc:    decl.Doc,
				Source: decl.Pos.File,
			})
		}
	}

	sort.Slice(infos, func(i, j int) bool {
		if infos[i].Module != infos[j].Module {
			return infos[i].Module < infos[j].Module
		}
		return infos[i].Name < infos[j].Name
	})
	return infos
}

// Modules возвращает список подключённых пакетов алиасов.
func (r *Registry) Modules() []string {
	packages := make([]string, len(r.order))
	copy(packages, r.order)
	sort.Strings(packages)
	return packages
}

// LoadFile разбирает один файл и регистрирует его алиасы в пространстве module.
// Возвращает инструкции верхнего уровня, если они есть.
func (r *Registry) LoadFile(module, path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать %s: %w", path, err)
	}

	file, err := Parse(path, string(data))
	if err != nil {
		return nil, err
	}
	if len(file.Imports) > 0 {
		// Второе имя из import в библиотеке стало бы видно всем сценариям.
		return nil, fmt.Errorf("%s: import пишут в сценарии; библиотека подключается целиком", file.Imports[0].Pos)
	}
	if err := r.AddFile(module, file); err != nil {
		return nil, err
	}
	return file, nil
}

// LoadDir подключает все файлы .ochan из директории как пакет module.
// У пакета (module не пуст) читается и манифест с версией.
func (r *Registry) LoadDir(module, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("не удалось прочитать %s: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == AliasExtension {
			names = append(names, entry.Name())
		}
	}
	// Порядок обхода директории не гарантирован, а от него зависит,
	// какая из двух одинаковых деклараций попадёт в сообщение об ошибке.
	sort.Strings(names)

	for _, name := range names {
		if _, err := r.LoadFile(module, filepath.Join(dir, name)); err != nil {
			return err
		}
	}

	if module == "" {
		return nil
	}
	pkg, err := LoadManifest(module, dir)
	if err != nil {
		return err
	}
	if pkg == nil {
		return nil
	}
	// Опечатка в prior.<алиас> молча оставила бы алиас без счёта автора.
	aliases := make([]string, 0, len(pkg.Priors))
	for alias := range pkg.Priors {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		if _, err := r.Lookup(module, alias); err != nil {
			return fmt.Errorf("%s: prior.%s: в пакете %s нет алиаса %s", pkg.Manifest, alias, module, alias)
		}
	}
	r.packages[module] = pkg
	return nil
}
