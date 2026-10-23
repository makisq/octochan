package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// slotKind — тип слота.
//
// Модель не собирает командную строку сама: она только заполняет слоты,
// а аргументы для ochan строит таблица. Значение, которое не проходит
// проверку своего типа, отвергается до запуска — это и есть детерминированная
// проверка между нейросетью и исполнением.
type slotKind int

const (
	// slotSource — конфигурация: файл внутри разрешённых каталогов или,
	// если разрешено, живой источник вида postgres://.
	slotSource slotKind = iota
	// slotFile — существующий файл внутри разрешённых каталогов.
	slotFile
	// slotNewDoc — новый документ .docx внутри разрешённых каталогов.
	// Существующий файл не перезаписывается никогда.
	slotNewDoc
	// slotParam — имя параметра конфигурации.
	slotParam
	// slotID — идентификатор записи журнала или заявки (или его начало).
	slotID
	// slotTaskID — идентификатор задачи во внешней системе.
	slotTaskID
	// slotPackage — имя пакета алиасов.
	slotPackage
	// slotLimit — положительное число записей.
	slotLimit
	// slotSwitch — флаг без значения.
	slotSwitch
	// slotOverrides — переопределения параметров сценария key=value.
	slotOverrides
	// slotName — имя сценария для сообщений, плана и заявки.
	slotName
	// slotText — текст формального промпта. Как и сценарий, идёт ochan
	// через stdin, но ничего не выполняет: его только проверяют.
	slotText
	// slotScript — текст сценария на языке алиасов. Передаётся ochan
	// через stdin, а не аргументом: сценарий бывает длинным, и в командной
	// строке ему не место — её видно в списке процессов.
	slotScript
)

// slot — именованное место, которое заполняет модель.
type slot struct {
	Name        string
	Kind        slotKind
	Description string
	Required    bool
	// Flag непуст, если значение передаётся флагом; иначе — позиционно.
	Flag string
}

// Шаблоны значений. Ни одно значение не может начинаться с дефиса: после
// «--» ochan и так не принял бы его за флаг, но имена параметров, задач
// и пакетов с дефиса не начинаются, и второй рубеж ничего не стоит.
var (
	paramPattern   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,127}$`)
	idPattern      = regexp.MustCompile(`^[0-9a-f]{1,32}$`)
	taskIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_\-]{0,63}$`)
	packagePattern = regexp.MustCompile(`^[\p{L}\p{N}_][\p{L}\p{N}_.\-]{0,63}$`)
	namePattern    = packagePattern
	overrideKey    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-]{0,63}$`)
	envPattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

const maxLimit = 1000

// maxScript — предел длины сценария. Сценарии пишутся под задачу и
// занимают десятки строк; предел отсекает мусор, а не настоящие сценарии.
const maxScript = 256 << 10

// errSlot — ошибка заполнения слота. Текст адресован модели: по нему она
// должна понять, что исправить, не гадая.
type errSlot struct {
	slot   string
	reason string
}

func (e *errSlot) Error() string { return fmt.Sprintf("%s: %s", e.slot, e.reason) }

func slotError(name, format string, args ...any) error {
	return &errSlot{slot: name, reason: fmt.Sprintf(format, args...)}
}

// sandbox задаёт, куда модели можно смотреть.
type sandbox struct {
	roots     []string
	liveAllow map[string]bool
	// env — переменные окружения, которые разрешено читать сценариям.
	env []string
}

// newSandbox проверяет корни и приводит их к реальным путям: иначе
// сравнение с путём, прошедшим через EvalSymlinks, давало бы ложные отказы.
func newSandbox(roots, live []string, env ...string) (*sandbox, error) {
	if len(roots) == 0 {
		return nil, errors.New("не задан ни один разрешённый каталог")
	}

	box := &sandbox{liveAllow: make(map[string]bool, len(live))}
	for _, name := range env {
		if name = strings.TrimSpace(name); name != "" {
			if !envPattern.MatchString(name) {
				return nil, fmt.Errorf("недопустимое имя переменной окружения %q", name)
			}
			box.env = append(box.env, name)
		}
	}
	sort.Strings(box.env)
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("каталог %s: %w", root, err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("каталог %s недоступен: %w", root, err)
		}
		info, err := os.Stat(real)
		if err != nil {
			return nil, fmt.Errorf("каталог %s недоступен: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s — не каталог", root)
		}
		box.roots = append(box.roots, real)
	}

	for _, scheme := range live {
		scheme = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(scheme), "://"))
		if scheme != "" {
			box.liveAllow[scheme] = true
		}
	}
	return box, nil
}

// ochanFlags — та же песочница в виде флагов ochan.
//
// Сценарий выполняет ochan, а не сервер, поэтому проверять пути внутри
// сценария серверу нечем: он передаёт свои ограничения интерпретатору,
// и встроенные функции языка проверяют их сами — при каждом обращении.
func (b *sandbox) ochanFlags() []string {
	flags := []string{"--sandbox"}
	for _, root := range b.roots {
		flags = append(flags, "--root="+root)
	}
	for _, scheme := range b.liveSchemes() {
		flags = append(flags, "--allow-live="+scheme)
	}
	for _, name := range b.env {
		flags = append(flags, "--allow-env="+name)
	}
	return flags
}

// liveSchemes возвращает разрешённые схемы живых источников по порядку.
func (b *sandbox) liveSchemes() []string {
	schemes := make([]string, 0, len(b.liveAllow))
	for scheme := range b.liveAllow {
		schemes = append(schemes, scheme)
	}
	sort.Strings(schemes)
	return schemes
}

// inside сообщает, что путь лежит внутри одного из корней.
func (b *sandbox) inside(path string) bool {
	for _, root := range b.roots {
		rel, err := filepath.Rel(root, path)
		if err != nil || filepath.IsAbs(rel) {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return true
	}
	return false
}

// resolve приводит путь к абсолютному и реальному.
//
// Относительный путь отсчитывается от первого корня. Символические ссылки
// раскрываются до проверки, иначе ссылка внутри каталога вела бы наружу.
func (b *sandbox) resolve(name, value string, mustExist bool) (string, error) {
	if value == "" {
		return "", slotError(name, "пустой путь")
	}
	if strings.ContainsRune(value, 0) {
		return "", slotError(name, "путь содержит нулевой символ")
	}

	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(b.roots[0], path)
	}
	path = filepath.Clean(path)

	var real string
	if mustExist {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", slotError(name, "файл %s не найден", value)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return "", slotError(name, "файл %s недоступен", value)
		}
		// Только обычные файлы: каталог, устройство или канал на месте
		// конфига — либо ошибка, либо зависание при чтении.
		if !info.Mode().IsRegular() {
			return "", slotError(name, "%s — не обычный файл", value)
		}
		real = resolved
	} else {
		dir, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return "", slotError(name, "каталог для %s не существует", value)
		}
		real = filepath.Join(dir, filepath.Base(path))
	}

	if !b.inside(real) {
		return "", slotError(name, "%s вне разрешённых каталогов (%s)",
			value, strings.Join(b.roots, ", "))
	}
	return real, nil
}

// source проверяет источник конфигурации.
func (b *sandbox) source(name, value string) (string, error) {
	if scheme, _, found := strings.Cut(value, "://"); found {
		scheme = strings.ToLower(scheme)
		if !b.liveAllow[scheme] {
			if len(b.liveAllow) == 0 {
				return "", slotError(name, "живые источники отключены; "+
					"сервер нужно запустить с --allow-live %s", scheme)
			}
			return "", slotError(name, "схема %s не разрешена, разрешены: %s",
				scheme, strings.Join(b.liveSchemes(), ", "))
		}
		return value, nil
	}
	return b.resolve(name, value, true)
}

// newDoc проверяет путь нового документа.
func (b *sandbox) newDoc(name, value string) (string, error) {
	if !strings.EqualFold(filepath.Ext(value), ".docx") {
		return "", slotError(name, "документ должен иметь расширение .docx")
	}
	path, err := b.resolve(name, value, false)
	if err != nil {
		return "", err
	}
	// Перезапись существующего файла — изменение, а изменения этот сервер
	// не делает. Модель может ошибиться с именем, и тогда пропал бы чужой
	// документ.
	if _, err := os.Lstat(path); err == nil {
		return "", slotError(name, "файл %s уже существует, перезапись запрещена", value)
	} else if !os.IsNotExist(err) {
		return "", slotError(name, "файл %s недоступен", value)
	}
	return path, nil
}

// fill проверяет один слот и возвращает его аргументы для ochan.
func (b *sandbox) fill(s slot, raw any) ([]string, error) {
	switch s.Kind {
	case slotSwitch:
		on, ok := raw.(bool)
		if !ok {
			return nil, slotError(s.Name, "ожидается true или false")
		}
		if !on {
			return nil, nil
		}
		return []string{"--" + s.Flag}, nil

	case slotLimit:
		number, ok := raw.(float64)
		if !ok || number != float64(int(number)) || number < 1 || number > maxLimit {
			return nil, slotError(s.Name, "ожидается целое число от 1 до %d", maxLimit)
		}
		return withFlag(s, strconv.Itoa(int(number))), nil

	case slotOverrides:
		return overrides(s.Name, raw)
	}

	text, ok := raw.(string)
	if !ok {
		return nil, slotError(s.Name, "ожидается строка")
	}

	var (
		value string
		err   error
	)
	switch s.Kind {
	case slotSource:
		value, err = b.source(s.Name, text)
	case slotFile:
		value, err = b.resolve(s.Name, text, true)
	case slotNewDoc:
		value, err = b.newDoc(s.Name, text)
	case slotParam:
		value, err = matched(s.Name, text, paramPattern, "латинские буквы, цифры, _ . -")
	case slotID:
		value, err = matched(s.Name, strings.ToLower(text), idPattern, "шестнадцатеричные цифры")
	case slotTaskID:
		value, err = matched(s.Name, text, taskIDPattern, "латинские буквы, цифры, _ -")
	case slotPackage:
		value, err = matched(s.Name, text, packagePattern, "буквы, цифры, _ . -")
	case slotName:
		value, err = matched(s.Name, text, namePattern, "буквы, цифры, _ . -")
	default:
		err = slotError(s.Name, "неизвестный тип слота")
	}
	if err != nil {
		return nil, err
	}
	return withFlag(s, value), nil
}

func withFlag(s slot, value string) []string {
	if s.Flag != "" {
		return []string{"--" + s.Flag + "=" + value}
	}
	return []string{value}
}

func matched(name, value string, pattern *regexp.Regexp, allowed string) (string, error) {
	if !pattern.MatchString(value) {
		return "", slotError(name, "недопустимое значение %q, разрешены: %s", value, allowed)
	}
	return value, nil
}

// stdinSlot сообщает, что значение слота передаётся ochan через stdin.
func stdinSlot(kind slotKind) bool { return kind == slotScript || kind == slotText }

// scriptText проверяет текст сценария.
//
// Сам сценарий проверяет ochan: разбор, имена, типы. Здесь отсекается то,
// что до ochan доходить не должно вовсе.
func scriptText(name string, raw any) (string, error) {
	text, ok := raw.(string)
	if !ok {
		return "", slotError(name, "ожидается текст строкой")
	}
	switch {
	case strings.TrimSpace(text) == "":
		return "", slotError(name, "пустой текст")
	case len(text) > maxScript:
		return "", slotError(name, "текст длиннее %d КиБ", maxScript>>10)
	case strings.ContainsRune(text, 0) || !utf8.ValidString(text):
		return "", slotError(name, "текст содержит недопустимые символы")
	}
	return text, nil
}

// overrides превращает объект в аргументы key=value.
//
// Ключи сортируются: одинаковый вызов всегда даёт одинаковую командную
// строку, а значит, одинаковый план и одинаковую заявку.
func overrides(name string, raw any) ([]string, error) {
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, slotError(name, "ожидается объект {параметр: значение}")
	}

	keys := make([]string, 0, len(object))
	for key := range object {
		if !overrideKey.MatchString(key) {
			return nil, slotError(name, "недопустимое имя параметра %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	args := make([]string, 0, len(keys))
	for _, key := range keys {
		var value string
		switch v := object[key].(type) {
		case string:
			value = v
		case bool:
			value = strconv.FormatBool(v)
		case float64:
			value = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			return nil, slotError(name, "значение %s должно быть строкой, числом или true/false", key)
		}
		if strings.ContainsRune(value, 0) || !utf8.ValidString(value) {
			return nil, slotError(name, "значение %s содержит недопустимые символы", key)
		}
		args = append(args, key+"="+value)
	}
	return args, nil
}
