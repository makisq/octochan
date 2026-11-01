package fp

import (
	"strings"
	"testing"
)

const incidentShape = `goal: stable

classes:
  Connection : "connection"
  Process    : "process"
  State      : "stable"

relations:
  Connection --limits--> Process (connection_limit)
  Process --strives_to--> State

cell c1:
  set Connection.maximum = 200
  set Process.load = high
`

// Структура без частных имён: та же запись с другими числами и ячейкой —
// одна структура; другое отношение — другая.
func TestShape(t *testing.T) {
	shape := func(src string) []string {
		prog, diags := Parse("incident.fp", src)
		if len(diags) > 0 {
			t.Fatalf("разбор: %v", diags)
		}
		return Shape(prog, func(w string) bool { return w == "high" })
	}
	a := shape(incidentShape)
	b := shape(strings.NewReplacer("200", "500", "cell c1", "cell db7").Replace(incidentShape))
	if s := Similarity(a, b); s != 1 {
		t.Fatalf("та же структура: %.2f\n%v\n%v", s, a, b)
	}
	c := shape(strings.Replace(incidentShape, "--limits-->", "--modifies-->", 1))
	if s := Similarity(a, c); s >= 1 || s <= 0.5 {
		t.Fatalf("другое отношение: %.2f", s)
	}
	if !strings.Contains(strings.Join(a, "|"), "set Connection.maximum #") ||
		!strings.Contains(strings.Join(a, "|"), "set Process.load high") {
		t.Fatalf("факты: %v", a)
	}
}
