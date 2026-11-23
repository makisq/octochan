package rbac

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Временные права, запреты и роли.
//
// Право на два часа, запрет на время инцидента, роль на полчаса — то,
// что иначе выдают и забывают отозвать. Срок хранится вместе с правом, и
// по его истечении право просто перестаёт действовать: отзывать нечего,
// и забыть отозвать нельзя. Выдают их те же команды с --for или --until —
// человек, CI или скрипт с правом admin; каждая выдача и каждое
// переключение — в журнале аудита, как любая команда.

// Now — часы, по которым сверяются сроки.
var Now = time.Now

// MaxTemporary — предел срока: бессрочное право выдают без --for.
const MaxTemporary = 30 * 24 * time.Hour

// TempPerm — право или запрет до момента Until.
type TempPerm struct {
	Perm  string
	Until time.Time
}

// ParseFor разбирает срок: 30m, 2h, 1d.
func ParseFor(text string) (time.Duration, error) {
	text = strings.TrimSpace(text)
	var d time.Duration
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, errors.New("срок — длительность вида 30m, 2h или 1d")
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(text); err != nil {
			return 0, errors.New("срок — длительность вида 30m, 2h или 1d")
		}
	}
	if d < time.Minute || d > MaxTemporary {
		return 0, fmt.Errorf("срок от 1m до %s", formatWindow(MaxTemporary))
	}
	return d, nil
}

// CheckUntil проверяет абсолютный срок.
func CheckUntil(until, now time.Time) error {
	switch {
	case !until.After(now):
		return fmt.Errorf("срок %s уже прошёл", until.Local().Format("2006-01-02 15:04"))
	case until.Sub(now) > MaxTemporary:
		return fmt.Errorf("срок дальше %s: бессрочное право выдают без --for и --until", formatWindow(MaxTemporary))
	}
	return nil
}

func active(list []TempPerm, now time.Time) []string {
	var perms []string
	for _, t := range list {
		if now.Before(t.Until) {
			perms = append(perms, t.Perm)
		}
	}
	return perms
}

// ActiveTemp — временные права роли, которые действуют сейчас.
func (r *Role) ActiveTemp() []TempPerm { return activeList(r.TempPerms) }

// ActiveTempDeny — временные запреты роли, которые действуют сейчас.
func (r *Role) ActiveTempDeny() []TempPerm { return activeList(r.TempDeny) }

func activeList(list []TempPerm) []TempPerm {
	now := Now()
	var out []TempPerm
	for _, t := range list {
		if now.Before(t.Until) {
			out = append(out, t)
		}
	}
	return out
}

// upsert добавляет право со сроком; повторная выдача того же права
// продлевает или сокращает срок до нового.
func upsert(list []TempPerm, perms []string, until time.Time) []TempPerm {
	for _, perm := range perms {
		found := false
		for i := range list {
			if list[i].Perm == perm {
				list[i].Until = until
				found = true
			}
		}
		if !found {
			list = append(list, TempPerm{Perm: perm, Until: until})
		}
	}
	return list
}

// GrantFor выдаёт роли права до момента until.
func (s *Store) GrantFor(name string, perms []string, until time.Time, extra []string) error {
	if err := CheckUntil(until, Now()); err != nil {
		return err
	}
	role, err := s.role(name)
	if err != nil {
		return err
	}
	if err := checkGrant(role, perms, extra); err != nil {
		return err
	}
	role.TempPerms = upsert(role.TempPerms, dedupe(perms), until)
	return nil
}

// DenyFor запрещает роли права до момента until: заморозка на время
// инцидента или релиза.
func (s *Store) DenyFor(name string, perms []string, until time.Time, extra []string) error {
	if err := CheckUntil(until, Now()); err != nil {
		return err
	}
	role, err := s.policyRole(name)
	if err != nil {
		return err
	}
	if err := CheckPerms(perms, extra); err != nil {
		return err
	}
	role.TempDeny = upsert(role.TempDeny, dedupe(perms), until)
	return nil
}

// Prune убирает истёкшие права и запреты: они уже не действуют, а
// хранить их незачем.
func (s *Store) Prune(now time.Time) {
	keep := func(list []TempPerm) []TempPerm {
		var out []TempPerm
		for _, t := range list {
			if now.Before(t.Until) {
				out = append(out, t)
			}
		}
		return out
	}
	for _, role := range s.Roles {
		role.TempPerms = keep(role.TempPerms)
		role.TempDeny = keep(role.TempDeny)
	}
}

// SwitchFor делает роль текущей до момента until; потом вернётся роль,
// которая была текущей до переключения.
func (s *Store) SwitchFor(name string, until time.Time) error {
	if err := CheckUntil(until, Now()); err != nil {
		return err
	}
	previous := s.Current
	if s.ReturnTo != "" {
		// Временная роль поверх временной: вернуться к исходной.
		previous = s.ReturnTo
	}
	if err := s.Switch(name); err != nil {
		return err
	}
	s.CurrentUntil, s.ReturnTo = until, previous
	return nil
}

// Expire возвращает роль, если срок текущей истёк. from — роль, срок
// которой истёк, to — ставшая текущей; ok — что-то изменилось.
func (s *Store) Expire(now time.Time) (from, to string, ok bool) {
	if s.CurrentUntil.IsZero() || now.Before(s.CurrentUntil) {
		return "", "", false
	}
	from = s.Current
	to = s.ReturnTo
	if s.Find(to) == nil {
		// Роль, к которой надо вернуться, удалили: самая безопасная —
		// llm, у неё ничего, кроме чтения и заявок.
		to = LLM
	}
	_ = s.Switch(to)
	return from, s.Current, true
}
