package lang

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AliasExtension — расширение файлов с алиасами.
const AliasExtension = ".ochan"

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
	// order хранит порядок подключения пакетов для разрешения коротких имён
	order []string
}

// NewRegistry создаёт пустой реестр.
func NewRegistry() *Registry {
	return &Registry{modules: map[string]map[string]*AliasDecl{"": {}}}
}

// Clone возвращает копию реестра. Объявления общие — их никто не
// меняет, — а пространства свои: алиасы, добавленные в копию, не видны
// в оригинале.
func (r *Registry) Clone() *Registry {
	clone := &Registry{
		modules: make(map[string]map[string]*AliasDecl, len(r.modules)),
		order:   append([]string(nil), r.order...),
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

	decl.Module = module
	space[decl.Name] = decl
	return nil
}

// Lookup ищет алиас. Пустой module означает поиск без префикса:
// сначала среди пользовательских, затем среди пакетов.
func (r *Registry) Lookup(module, name string) (*AliasDecl, error) {
	if module != "" {
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

	for _, decl := range file.Aliases {
		if err := r.Add(module, decl); err != nil {
			return nil, err
		}
	}
	return file, nil
}

// LoadDir подключает все файлы .ochan из директории как пакет module.
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
	return nil
}
