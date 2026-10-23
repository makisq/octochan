package lang

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sandbox ограничивает то, до чего дотягиваются встроенные функции.
//
// Без песочницы сценарий умеет всё, что умеет запустивший его человек:
// читать и писать любые файлы, читать любые переменные окружения и ходить
// к живым серверам. Для собственных сценариев это нормально. Для сценария,
// который написала модель, — нет: среди переменных окружения лежат токен
// API и пароли, а запись в произвольный путь обходит все проверки сервера
// MCP. Поэтому в песочнице:
//
//   - файлы — только внутри разрешённых каталогов, символические ссылки
//     раскрываются до проверки;
//   - переменные окружения — только из белого списка;
//   - живые источники — только разрешённых схем.
//
// Отказ — ошибка с объяснением, а не тихое пустое значение: сценарий,
// которому подсунули пустую строку вместо пароля, сломается дальше и
// непонятно где.
type Sandbox struct {
	roots []string
	env   map[string]bool
	live  map[string]bool
}

// NewSandbox создаёт песочницу. Каталоги приводятся к реальным путям:
// иначе сравнение с путём, прошедшим через EvalSymlinks, давало бы ложные
// отказы.
func NewSandbox(roots, env, live []string) (*Sandbox, error) {
	if len(roots) == 0 {
		return nil, errors.New("песочнице не задан ни один разрешённый каталог")
	}

	box := &Sandbox{env: make(map[string]bool), live: make(map[string]bool)}
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

	for _, name := range env {
		if name = strings.TrimSpace(name); name != "" {
			box.env[name] = true
		}
	}
	for _, scheme := range live {
		scheme = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(scheme), "://"))
		if scheme != "" {
			box.live[scheme] = true
		}
	}
	return box, nil
}

// Roots возвращает разрешённые каталоги.
func (s *Sandbox) Roots() []string { return append([]string(nil), s.roots...) }

// Env возвращает разрешённые переменные окружения по порядку.
func (s *Sandbox) Env() []string { return sortedKeys(s.env) }

// Live возвращает разрешённые схемы живых источников по порядку.
func (s *Sandbox) Live() []string { return sortedKeys(s.live) }

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// inside сообщает, что путь лежит внутри одного из разрешённых каталогов.
func (s *Sandbox) inside(path string) bool {
	for _, root := range s.roots {
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

// resolve проверяет абсолютный путь и возвращает его реальный вид.
func (s *Sandbox) resolve(path string) (string, error) {
	if strings.ContainsRune(path, 0) {
		return "", errors.New("путь содержит нулевой символ")
	}

	real, err := realPath(path)
	if err != nil {
		return "", fmt.Errorf("путь %s недоступен: %w", path, err)
	}
	if !s.inside(real) {
		return "", fmt.Errorf("%s вне разрешённых каталогов (%s)", path, strings.Join(s.roots, ", "))
	}
	return real, nil
}

// RealPath раскрывает символические ссылки в пути, в том числе ещё не
// существующем, — так же, как песочница перед проверкой.
func RealPath(path string) (string, error) { return realPath(path) }

// realPath раскрывает символические ссылки в самой длинной существующей
// части пути и дописывает к ней остаток.
//
// Файл, который сценарий только собирается создать, ещё не существует,
// и раскрыть весь путь нельзя. Но каталог над ним существует, и именно он
// может оказаться ссылкой наружу.
func realPath(path string) (string, error) {
	existing := path
	var rest []string
	for {
		real, err := filepath.EvalSymlinks(existing)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		// Ссылка, ведущая в никуда, раскрыться не может. Запись по ней
		// создала бы файл там, куда она указывает, — возможно, снаружи.
		if info, err := os.Lstat(existing); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("символическая ссылка %s никуда не ведёт", existing)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return path, nil
		}
		rest = append(rest, filepath.Base(existing))
		existing = parent
	}
}

// checkEnv проверяет, можно ли читать переменную окружения.
func (s *Sandbox) checkEnv(name string) error {
	if s.env[name] {
		return nil
	}
	if len(s.env) == 0 {
		return fmt.Errorf("переменная окружения %s недоступна: в песочнице не разрешена ни одна", name)
	}
	return fmt.Errorf("переменная окружения %s недоступна, разрешены: %s", name, strings.Join(s.Env(), ", "))
}

// checkLive проверяет, можно ли обращаться к живому источнику этой схемы.
func (s *Sandbox) checkLive(scheme string) error {
	if s.live[scheme] {
		return nil
	}
	if len(s.live) == 0 {
		return fmt.Errorf("живые источники в песочнице отключены (схема %s)", scheme)
	}
	return fmt.Errorf("схема %s не разрешена, разрешены: %s", scheme, strings.Join(s.Live(), ", "))
}
