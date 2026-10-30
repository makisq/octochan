package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Свободный уровень (free) — контур без человека.
//
// На узле HA-AI человека-оператора нет: некому согласовать заявку модели и
// некому её выполнить. master — единственный человек, у которого есть все
// права, — передаёт модели часть их: роль, чьи права на выполнение она
// берёт, политику — что выполнять без человека, — и срок. Заявка модели в
// политике согласуется ею и выполняется сразу; вне политики — ждёт master.
// free не даёт прав и не снимает проверок: песочница, кольца, арбитр и
// сверка плана действуют как всегда — меняется только то, кто сказал «да».

// FreeGrant — выдача free.
type FreeGrant struct {
	// Agent — агент, которому выдано: имя в журнале и в отзыве.
	Agent string `json:"agent"`
	// Role — чьи права модель берёт на выполнение; Policy — что
	// выполняется без человека.
	Role   string    `json:"role"`
	Policy string    `json:"policy"`
	Window string    `json:"window"`
	Start  time.Time `json:"start"`
	Until  time.Time `json:"until"`
	By     string    `json:"by"`
	// Ended и Outcome — когда и почему выдача кончилась.
	Ended   time.Time `json:"ended,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
}

// Open — выдача действует в момент now.
func (g *FreeGrant) Open(now time.Time) bool { return g.Ended.IsZero() && now.Before(g.Until) }

// FreeGrants — выдачи free в одном файле.
type FreeGrants struct{ path string }

// NewFreeGrants открывает хранилище выдач.
func NewFreeGrants(path string) *FreeGrants { return &FreeGrants{path: path} }

// List — все выдачи от старых к новым.
func (f *FreeGrants) List() ([]FreeGrant, error) {
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var grants []FreeGrant
	if err := json.Unmarshal(data, &grants); err != nil {
		return nil, fmt.Errorf("повреждён файл выдач free %s: %w", f.path, err)
	}
	return grants, nil
}

func (f *FreeGrants) update(change func([]FreeGrant) ([]FreeGrant, error)) error {
	unlock, err := lockPath(f.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	grants, err := f.List()
	if err != nil {
		return err
	}
	if grants, err = change(grants); err != nil {
		return err
	}
	if len(grants) > keepWindows {
		grants = grants[len(grants)-keepWindows:]
	}
	return writeJSONAtomic(f.path, grants)
}

// Grant записывает выдачу. Действующая выдача агента не продлевается
// молча: её отзывают и выдают заново.
func (f *FreeGrants) Grant(g FreeGrant) error {
	return f.update(func(grants []FreeGrant) ([]FreeGrant, error) {
		for _, old := range grants {
			if old.Agent == g.Agent && old.Open(g.Start) {
				return nil, fmt.Errorf("free агента %s уже действует до %s (роль %s, политика %s): отзовите и выдайте заново",
					g.Agent, old.Until.Local().Format("15:04"), old.Role, old.Policy)
			}
		}
		return append(grants, g), nil
	})
}

// Revoke закрывает действующую выдачу агента.
func (f *FreeGrants) Revoke(agent, outcome string, now time.Time) (*FreeGrant, error) {
	var ended *FreeGrant
	err := f.update(func(grants []FreeGrant) ([]FreeGrant, error) {
		for i := range grants {
			g := &grants[i]
			if g.Agent == agent && g.Open(now) {
				g.Ended, g.Outcome = now, outcome
				copy := *g
				ended = &copy
				return grants, nil
			}
		}
		return nil, fmt.Errorf("у агента %s нет действующего free", agent)
	})
	return ended, err
}

// Settle закрывает истёкшие выдачи и возвращает их.
func (f *FreeGrants) Settle(now time.Time) ([]FreeGrant, error) {
	grants, err := f.List()
	if err != nil {
		return nil, err
	}
	due := false
	for _, g := range grants {
		if g.Ended.IsZero() && !now.Before(g.Until) {
			due = true
		}
	}
	if !due {
		return nil, nil
	}
	var expired []FreeGrant
	err = f.update(func(grants []FreeGrant) ([]FreeGrant, error) {
		for i := range grants {
			g := &grants[i]
			if g.Ended.IsZero() && !now.Before(g.Until) {
				g.Ended, g.Outcome = g.Until, fmt.Sprintf("истёк (%s) — isolation вернулось в natural", g.Window)
				expired = append(expired, *g)
			}
		}
		return grants, nil
	})
	return expired, err
}

// Active — действующие выдачи.
func Active(grants []FreeGrant, now time.Time) []FreeGrant {
	var out []FreeGrant
	for _, g := range grants {
		if g.Open(now) {
			out = append(out, g)
		}
	}
	return out
}
