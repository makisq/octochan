package workspace

import (
	"strings"
	"testing"
	"time"
)

func TestGraph(t *testing.T) {
	at := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	req := func(id string, state RequestState, after ...string) *Request {
		at = at.Add(time.Minute)
		return &Request{ID: id, State: state, After: after, Created: at}
	}
	a := req("a", RequestApplied)
	b := req("b", RequestApproved, "a")
	c := req("c", RequestPending, "b")
	d := req("d", RequestApproved, "x")
	e := req("e", RequestApproved, "a", "b")
	g := NewGraph([]*Request{a, b, c, d, e})

	if !g.Readiness(b).Ready() {
		t.Fatal("b после выполненной a не готова")
	}
	if got := g.Status(c); got != "ждёт решения (зависит от b)" {
		t.Fatalf("c: %q", got)
	}
	if got := g.Status(e); got != "ожидает (зависит от b)" {
		t.Fatalf("e: %q", got)
	}
	if got := g.Status(d); !strings.Contains(got, "не выполнится (x — такой заявки нет)") {
		t.Fatalf("d: %q", got)
	}
	ready := g.Ready()
	if len(ready) != 1 || ready[0] != b {
		t.Fatalf("готовые: %v", ready)
	}

	// b упала: c и e не выполнятся, по цепочке тоже.
	b.State = RequestFailed
	f := req("f", RequestPending, "c")
	g = NewGraph([]*Request{a, b, c, d, e, f})
	if got := g.Status(f); got != "не выполнится (c не выполнится: b упала)" {
		t.Fatalf("f: %q", got)
	}
	var ids []string
	for _, r := range g.Downstream("b") {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "c,e,f" {
		t.Fatalf("после b: %v", ids)
	}

	// Сделанное a отменено обратным действием: заявки после неё — тоже.
	undo := req("u", RequestApplied)
	a.CompensatedBy = "u"
	h := req("h", RequestApproved, "a")
	g = NewGraph([]*Request{a, undo, h})
	if got := g.Status(h); got != "не выполнится (a отменена обратным действием u)" {
		t.Fatalf("h: %q", got)
	}

	// Испорченные файлы с циклом не вешают проверку.
	x := req("x", RequestPending, "y")
	y := req("y", RequestPending, "x")
	g = NewGraph([]*Request{x, y})
	if got := g.Status(x); !strings.Contains(got, "цикл") {
		t.Fatalf("цикл: %q", got)
	}
}
