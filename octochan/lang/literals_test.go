package lang

import (
	"testing"
)

func TestLiteralWatch(t *testing.T) {
	for in, want := range map[Value]Value{
		IntValue(200): IntValue(201), FloatValue(0.5): FloatValue(1.5),
		StringValue("200"): StringValue("201"), StringValue("16MB"): StringValue("17MB"),
		StringValue("30 s"): StringValue("31 s"), StringValue("on"): StringValue("on"),
		StringValue("10.0.0.1"): StringValue("10.0.0.1"), StringValue("prod.conf"): StringValue("prod.conf"),
	} {
		if got := Perturb(in); got != want {
			t.Errorf("Perturb(%v) = %v, ожидалось %v", in, got, want)
		}
	}

	src := "x = \"100\"\ny = int(x) * 2\nprint(str(y))\nparallel for i in [1, 2] {\n    print(\"on\")\n}\n"
	file, err := Parse("s.ochan", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, perturb := range []bool{false, true} {
		var out stringsWriter
		in := NewInterpreter(nil, NewRegistry(), &out)
		in.WatchLiterals("s.ochan", perturb)
		if err := in.RunFile(file); err != nil {
			t.Fatal(err)
		}
		want := "200\non\non\n"
		if perturb {
			want = "303\non\non\n" // (100+1) * (2+1)
		}
		if out.String() != want {
			t.Errorf("perturb=%v: вывод %q", perturb, out.String())
		}
		lits := in.Literals()
		if len(lits) != 4 || lits[0] != "100" || lits[3] != "on" { // без повторов: 2 уже был
			t.Errorf("литералы: %v", lits)
		}
	}
}

type stringsWriter struct{ b []byte }

func (w *stringsWriter) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }
func (w *stringsWriter) String() string              { return string(w.b) }
