package workspace

import (
	"fmt"
	"sort"
	"strings"
)

// Порядок между заявками.
//
// Заявка B, оформленная с --after A, выполняется только после A. Ядро
// держит порядок и ничего больше: повтор, расписание и продолжение с места
// — дело модуля рабочего процесса, который выполняет готовые заявки
// (Graph.Ready) в своём темпе.
//
// Сбой. A выполнена, B ждала A и упала.
//   - A остаётся. Сбой B её не откатывает: A согласовали и выполнили
//     отдельно, её результат верен сам по себе. Вернуть её — отдельное
//     решение: ochan rollback или ochan compensate.
//   - B не повторяют. Она упала посреди выполнения, и согласованный план
//     уже не описывает систему. B пересобирают — новой заявкой с --after A,
//     и её согласуют заново.
//   - Заявки после B не выполнятся: они строились в расчёте на B. Их тоже
//     оформляют заново, после новой B.
//
// Цикла быть не может: --after называет только уже существующие заявки,
// поэтому ребро всегда ведёт к более старой.

// Graph — заявки по идентификатору.
type Graph map[string]*Request

// NewGraph строит граф из списка заявок.
func NewGraph(all []*Request) Graph {
	g := make(Graph, len(all))
	for _, r := range all {
		g[r.ID] = r
	}
	return g
}

// Blocker — зависимость, из-за которой заявка не выполнится никогда.
type Blocker struct {
	ID     string
	Reason string
}

func (b Blocker) String() string { return b.ID + " " + b.Reason }

// Readiness — что держит заявку.
type Readiness struct {
	// Waiting — зависимости, которые ещё не выполнены, но могут быть.
	Waiting []string
	// Broken — зависимости, которые не выполнятся или чьё сделанное
	// отменено: заявка, построенная в расчёте на них, не выполнится.
	Broken []Blocker
}

// Ready сообщает, что зависимостей, которые держат заявку, нет.
func (r Readiness) Ready() bool { return len(r.Waiting) == 0 && len(r.Broken) == 0 }

// Readiness — что держит заявку: невыполненные и сломанные зависимости.
func (g Graph) Readiness(request *Request) Readiness {
	var out Readiness
	for _, id := range request.After {
		dep, ok := g[id]
		if !ok {
			out.Broken = append(out.Broken, Blocker{id, "— такой заявки нет"})
			continue
		}
		if reason := g.broken(dep, map[string]bool{request.ID: true}); reason != "" {
			out.Broken = append(out.Broken, Blocker{id, reason})
			continue
		}
		if dep.State != RequestApplied {
			out.Waiting = append(out.Waiting, id)
		}
	}
	return out
}

// broken объясняет, почему заявка не выполнится или её сделанное
// отменено; пусто — с ней всё в порядке. Проверка идёт по цепочке:
// заявка после сломанной тоже сломана.
func (g Graph) broken(r *Request, seen map[string]bool) string {
	if seen[r.ID] {
		return "— зависимости заявок замкнулись в цикл"
	}
	seen[r.ID] = true
	defer delete(seen, r.ID)

	switch r.State {
	case RequestRejected:
		return "отклонена"
	case RequestFailed:
		return "упала"
	case RequestApplied:
		if c, ok := g[r.CompensatedBy]; ok && c.State == RequestApplied {
			return "отменена обратным действием " + c.ID
		}
		return ""
	}
	for _, id := range r.After {
		dep, ok := g[id]
		if !ok {
			return "не выполнится: заявки " + id + " нет"
		}
		if reason := g.broken(dep, seen); reason != "" {
			return fmt.Sprintf("не выполнится: %s %s", id, reason)
		}
	}
	return ""
}

// Dependents — заявки, которые прямо зависят от этой, от старых к новым.
func (g Graph) Dependents(id string) []*Request {
	var out []*Request
	for _, r := range g {
		for _, dep := range r.After {
			if dep == id {
				out = append(out, r)
				break
			}
		}
	}
	sortByCreated(out)
	return out
}

// Downstream — все заявки после этой, прямо и через другие, кроме
// конечных (выполненных, отклонённых, упавших), от старых к новым.
func (g Graph) Downstream(id string) []*Request {
	seen := map[string]bool{}
	var out []*Request
	var walk func(string)
	walk = func(id string) {
		for _, r := range g.Dependents(id) {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			if r.State == RequestPending || r.State == RequestApproved {
				out = append(out, r)
			}
			walk(r.ID)
		}
	}
	walk(id)
	sortByCreated(out)
	return out
}

// Ready — согласованные заявки, которые можно выполнять сейчас: все их
// зависимости выполнены. Порядок — от старых к новым; модуль рабочего
// процесса выполняет их и спрашивает снова.
func (g Graph) Ready() []*Request {
	var out []*Request
	for _, r := range g {
		if r.State == RequestApproved && g.Readiness(r).Ready() {
			out = append(out, r)
		}
	}
	sortByCreated(out)
	return out
}

// Status — состояние заявки с учётом порядка: «ожидает (зависит от …)»
// или «не выполнится (… упала)» для заявки, которую держат зависимости.
func (g Graph) Status(r *Request) string {
	if r.State != RequestPending && r.State != RequestApproved {
		return r.State.Russian()
	}
	readiness := g.Readiness(r)
	switch {
	case len(readiness.Broken) > 0:
		reasons := make([]string, len(readiness.Broken))
		for i, b := range readiness.Broken {
			reasons[i] = b.String()
		}
		return fmt.Sprintf("не выполнится (%s)", strings.Join(reasons, "; "))
	case len(readiness.Waiting) > 0 && r.State == RequestApproved:
		return fmt.Sprintf("ожидает (зависит от %s)", strings.Join(readiness.Waiting, ", "))
	case len(readiness.Waiting) > 0:
		return fmt.Sprintf("%s (зависит от %s)", r.State.Russian(), strings.Join(readiness.Waiting, ", "))
	}
	return r.State.Russian()
}

// DescribeIn — строка списка заявок с учётом порядка.
func (g Graph) DescribeIn(r *Request) string {
	parts := []string{
		r.Created.Local().Format("2006-01-02 15:04"),
		r.ID,
		fmt.Sprintf("%-12s", g.Status(r)),
	}
	if r.Service != "" {
		parts = append(parts, r.Service)
	} else if r.Target != "" {
		parts = append(parts, r.Target)
	}
	parts = append(parts, r.Summary)
	return strings.Join(parts, "  ")
}

func sortByCreated(list []*Request) {
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Created.Equal(list[j].Created) {
			return list[i].Created.Before(list[j].Created)
		}
		return list[i].ID < list[j].ID
	})
}
