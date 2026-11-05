package own

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"ochan/rings"
)

// rings.json, записанный прежней моделью (testdata/rings-py.json), читается
// и действует так же: надёжность правил — по журналу и по кластеру,
// исходы кластера, оценка правила выбора с калибровкой.
func TestParamsFromModel(t *testing.T) {
	dir := t.TempDir()
	data, err := os.ReadFile("testdata/rings-py.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ParamsFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadParams(dir)
	if err != nil {
		t.Fatal(err)
	}
	near := func(what string, got rings.Interval, l, u float64) {
		t.Helper()
		if math.Abs(got.L-l) > 1e-12 || math.Abs(got.U-u) > 1e-12 {
			t.Fatalf("%s: %+v, ожидалось [%v; %v]", what, got, l, u)
		}
	}
	c := p.Clusters["75cf2d265cfe"]
	if c == nil || c.Instances.N() != 3 {
		t.Fatalf("кластер: %+v", c)
	}
	near("по журналу", p.Rule("fp.modifies", nil), 0.4099252381720746, 0.827014145210248)
	near("по кластеру", p.Rule("fp.modifies", c), 0.39032691304402456, 0.843675360814715)
	near("исходы кластера", p.Outcomes(c), 0.39032691304402456, 0.843675360814715)
	if s := p.Score(rings.Features{LogProb: -0.5, Lower: 0.6, Width: 0.3, Binding: 1}); math.Abs(s-0.75) > 1e-12 {
		t.Fatalf("оценка %v", s)
	}
	if key, found := p.ClusterOf([]string{"class Config", "relation Limit modifies Config"}, nil); found != c || key != "75cf2d265cfe" {
		t.Fatalf("кластер задачи: %s", key)
	}
	if _, found := p.ClusterOf([]string{"class Other"}, nil); found != nil {
		t.Fatal("непохожая задача узнала кластер")
	}
}

// Каждая запись — новая версия; откат — тоже версия.
func TestParamsVersions(t *testing.T) {
	dir := t.TempDir()
	p := DefaultParams()
	if v, err := p.Save(dir, "первая"); err != nil || v != 1 {
		t.Fatalf("версия %d: %v", v, err)
	}
	p.WellTrue = 0.6
	if v, _ := p.Save(dir, "вторая"); v != 2 {
		t.Fatalf("версия %d", v)
	}
	to, n, err := Rollback(dir, -1)
	if err != nil || to != 1 || n != 3 {
		t.Fatalf("откат к %d, новая %d: %v", to, n, err)
	}
	back, _ := LoadParams(dir)
	if back.WellTrue != 0.5 {
		t.Fatalf("после отката порог %v", back.WellTrue)
	}
	log, _ := Versions(dir)
	if len(log) != 3 || log[2].Note != "откат к версии 1" {
		t.Fatalf("версии: %+v", log)
	}
	if _, _, err := Rollback(dir, 9); err == nil {
		t.Fatal("откат к несуществующей версии")
	}
}
