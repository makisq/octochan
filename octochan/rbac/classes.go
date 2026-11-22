package rbac

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ClassAnnotation — аннотация, которой команда модуля объявляет свой класс:
//
//	cmd.Annotations = map[string]string{"class": "read"}
//
// Класс — имя группы прав: read, plan, request, net, apply, install… Право
// на группу открывает и команды модулей этого класса — ядро править не
// нужно. Аннотации приходят по gRPC вместе с командой, поэтому класс
// известен, как только модуль загружен, и меняется при его перезагрузке.
const ClassAnnotation = "class"

// classAliases — другие имена классов: «execution» — исполнительная
// команда, как apply.
var classAliases = map[string]string{
	"execution": "apply",
	"exec":      "apply",
	"execute":   "apply",
	"write":     "apply",
}

var (
	classMu sync.RWMutex
	// classes — классы команд модулей: путь команды через точку → группа.
	classes = map[string]string{}
)

// NormalizeClass приводит объявленный класс к имени группы прав.
func NormalizeClass(class string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(class))
	if alias, ok := classAliases[name]; ok {
		name = alias
	}
	if _, ok := Groups[name]; !ok {
		return "", fmt.Errorf("неизвестный класс %q: группы прав %s (execution — то же, что apply)",
			class, strings.Join(GroupNames(), ", "))
	}
	return name, nil
}

// SetClasses заменяет классы команд модулей целиком: так перезагрузка
// модуля, убравшая аннотацию, убирает и класс. Команда ядра (она названа
// в группе) класса из модуля не получает — модуль не может переписать её
// права. Возвращает команды, которым класс не назначен, с причиной.
func SetClasses(next map[string]string) []string {
	fresh := make(map[string]string, len(next))
	var skipped []string
	for _, command := range sortedKeys(next) {
		if inGroups(command) {
			skipped = append(skipped, fmt.Sprintf("%s: команда ядра, её права задаёт ядро", command))
			continue
		}
		class, err := NormalizeClass(next[command])
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", command, err))
			continue
		}
		fresh[command] = class
	}
	classMu.Lock()
	classes = fresh
	classMu.Unlock()
	return skipped
}

// Classes — копия классов команд модулей.
func Classes() map[string]string {
	classMu.RLock()
	defer classMu.RUnlock()
	out := make(map[string]string, len(classes))
	for k, v := range classes {
		out[k] = v
	}
	return out
}

// ClassOf — класс команды модуля: свой, а у подкоманды без своего — класс
// команды верхнего уровня. Пусто — класса нет.
func ClassOf(command string) string {
	classMu.RLock()
	defer classMu.RUnlock()
	if class, ok := classes[command]; ok {
		return class
	}
	if i := strings.IndexByte(command, '.'); i > 0 {
		return classes[command[:i]]
	}
	return ""
}

// ownClass — у команды объявлен свой класс (не унаследованный).
func ownClass(command string) bool {
	classMu.RLock()
	defer classMu.RUnlock()
	_, ok := classes[command]
	return ok
}

func inGroups(command string) bool {
	for _, members := range Groups {
		if contains(members, command) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
