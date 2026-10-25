package rbac

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Политика роли сверх прав: запреты, область и пределы частоты.
//
// Права отвечают на вопрос «какие команды». Против чужого человека на
// общем сервере и против нейросети, которой подсунули вредную инструкцию,
// этого мало: роли с правом apply доступен любой файл сервера, а роль llm
// может завалить людей заявками. Поэтому:
//
//   - запрет перекрывает право: all минус serve пишется как all и запрет
//     serve, а не перечислением всего остального;
//   - область — каталоги, вне которых роль не работает: ни текущий
//     каталог, ни пути в аргументах, ни песочница её сценариев за них не
//     выходят;
//   - предел — сколько раз за окно времени роль может выполнить команды
//     права: apply=10/1h. Считается по журналу аудита.
//
// master — владелец: ни запретов, ни области, ни пределов у него не бывает,
// иначе одна опечатка запирала бы его снаружи без способа вернуться.

// Limit — предел частоты: не больше Count команд права Perm за Window.
type Limit struct {
	Perm   string
	Count  int
	Window time.Duration
}

// Пределы окна: меньше минуты — это уже не предел частоты, а запрет;
// больше месяца — журнал за такой срок читать слишком долго.
const (
	minWindow = time.Minute
	maxWindow = 31 * 24 * time.Hour
)

func (l Limit) String() string {
	return fmt.Sprintf("%s=%d/%s", l.Perm, l.Count, formatWindow(l.Window))
}

// formatWindow пишет окно коротко: 1h, 30m, 2d.
func formatWindow(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	default:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
}

// ParseLimit разбирает предел вида apply=10/1h. Окно — длительность Go
// (30m, 1h) или дни (1d).
func ParseLimit(text string) (Limit, error) {
	perm, rest, ok := strings.Cut(strings.TrimSpace(text), "=")
	count, window, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || perm == "" {
		return Limit{}, fmt.Errorf("предел %q: нужно право=число/окно, например apply=10/1h", text)
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 1 {
		return Limit{}, fmt.Errorf("предел %q: число — целое от 1", text)
	}
	d, err := parseWindow(window)
	if err != nil {
		return Limit{}, fmt.Errorf("предел %q: %w", text, err)
	}
	return Limit{Perm: perm, Count: n, Window: d}, nil
}

func parseWindow(text string) (time.Duration, error) {
	var d time.Duration
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, errors.New("окно — длительность вида 30m, 1h или 2d")
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(text); err != nil {
			return 0, errors.New("окно — длительность вида 30m, 1h или 2d")
		}
	}
	if d < minWindow || d > maxWindow {
		return 0, fmt.Errorf("окно от %s до %s", formatWindow(minWindow), formatWindow(maxWindow))
	}
	return d, nil
}

// LimitsFor — пределы роли, под которые попадает команда.
func (r *Role) LimitsFor(command string) []Limit {
	var limits []Limit
	for _, limit := range r.Limits {
		if Covers(limit.Perm, command) {
			limits = append(limits, limit)
		}
	}
	return limits
}

// MaxWindow — самое длинное окно пределов роли; 0 — пределов нет.
func (r *Role) MaxWindow() time.Duration {
	var longest time.Duration
	for _, limit := range r.Limits {
		longest = max(longest, limit.Window)
	}
	return longest
}

// Scoped сообщает, что у роли есть область.
func (r *Role) Scoped() bool { return len(r.Paths) > 0 }

// InScope сообщает, что путь лежит в области роли. Путь — абсолютный и
// уже с раскрытыми символическими ссылками: раскрывает вызывающий, у него
// доступ к файловой системе.
func (r *Role) InScope(path string) bool {
	if !r.Scoped() {
		return true
	}
	for _, dir := range r.Paths {
		if Inside(dir, path) {
			return true
		}
	}
	return false
}

// Inside сообщает, что path — это dir или лежит внутри него.
func Inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// policyRole находит роль, которой можно задавать политику.
func (s *Store) policyRole(name string) (*Role, error) {
	role, err := s.role(name)
	if err != nil {
		return nil, err
	}
	if role.Name == Master {
		return nil, errors.New("master — владелец: запретов, области и пределов у него не бывает, " +
			"иначе одна ошибка заперла бы его без способа вернуться")
	}
	return role, nil
}

// DenyPerms запрещает роли права.
func (s *Store) DenyPerms(name string, perms, extra []string) error {
	role, err := s.policyRole(name)
	if err != nil {
		return err
	}
	if err := CheckPerms(perms, extra); err != nil {
		return err
	}
	role.Deny = dedupe(append(role.Deny, perms...))
	return nil
}

// Undeny снимает запреты.
func (s *Store) Undeny(name string, perms []string) error {
	role, err := s.role(name)
	if err != nil {
		return err
	}
	var kept []string
	for _, perm := range role.Deny {
		if !contains(perms, perm) {
			kept = append(kept, perm)
		}
	}
	temp := withoutPerms(role.TempDeny, perms)
	if len(kept) == len(role.Deny) && len(temp) == len(role.TempDeny) {
		return fmt.Errorf("у роли %s нет таких запретов: %s", role.Name, strings.Join(perms, ", "))
	}
	role.Deny, role.TempDeny = kept, temp
	return nil
}

// SetScope задаёт область роли; пустой список снимает её. Каталоги —
// абсолютные и с раскрытыми ссылками.
func (s *Store) SetScope(name string, dirs []string) error {
	role, err := s.policyRole(name)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("каталог области %s не абсолютный", dir)
		}
	}
	role.Paths = dedupe(dirs)
	return nil
}

// SetLimits задаёт пределы роли; пустой список снимает их. Предел того
// же права заменяет прежний.
func (s *Store) SetLimits(name string, limits []Limit, extra []string) error {
	role, err := s.policyRole(name)
	if err != nil {
		return err
	}
	if len(limits) == 0 {
		role.Limits = nil
		return nil
	}
	for _, limit := range limits {
		if err := CheckPerms([]string{limit.Perm}, extra); err != nil {
			return err
		}
	}
	merged := append([]Limit(nil), role.Limits...)
	for _, limit := range limits {
		replaced := false
		for i := range merged {
			if merged[i].Perm == limit.Perm {
				merged[i], replaced = limit, true
			}
		}
		if !replaced {
			merged = append(merged, limit)
		}
	}
	role.Limits = merged
	return nil
}
