package rings

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// Калибровка и веса правила выбора совпадают с прежней реализацией в коде
// своей модели (testdata/py-calibration.json — её выгрузка): параметры,
// пересчитанные ею, читаются и действуют так же.
func TestCalibrationMatchesModelParams(t *testing.T) {
	data, err := os.ReadFile("testdata/py-calibration.json")
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Features    [][4]float64 `json:"features"`
		Outcomes    []int        `json:"outcomes"`
		Weights     [5]float64   `json:"weights"`
		Raw         []float64    `json:"raw"`
		Calibration [][2]float64 `json:"calibration"`
		Applied     []float64    `json:"applied"`
		Probe       []float64    `json:"probe"`
		Scores      []float64    `json:"scores"`
		Share       [][2]float64 `json:"share"`
		Reliability [][2]float64 `json:"reliability"`
		Few         *[5]float64  `json:"few"`
	}
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	near := func(what string, a, b float64) {
		t.Helper()
		if math.Abs(a-b) > 1e-9 {
			t.Fatalf("%s: %.12f, ожидалось %.12f", what, a, b)
		}
	}
	var fs []Features
	var ok []bool
	for i, f := range want.Features {
		fs = append(fs, Features{f[0], f[1], f[2], f[3]})
		ok = append(ok, want.Outcomes[i] == 1)
	}
	w, fitted := FitWeights(fs, ok)
	if !fitted {
		t.Fatal("веса не пересчитаны")
	}
	for i, got := range []float64{w.Bias, w.LogProb, w.Lower, w.Width, w.Binding} {
		near("вес", got, want.Weights[i])
	}
	for i, f := range fs {
		near("оценка до калибровки", w.Probability(f, nil), want.Raw[i])
	}
	cal := Isotonic(want.Raw, ok, 10)
	if len(cal) != len(want.Calibration) {
		t.Fatalf("точек калибровки %d, ожидалось %d", len(cal), len(want.Calibration))
	}
	for i, p := range cal {
		near("точка", p.Score, want.Calibration[i][0])
		near("доля", p.Observed, want.Calibration[i][1])
	}
	for i, s := range want.Probe {
		near("поправка", cal.Apply(s), want.Applied[i])
	}
	for i := range want.Scores {
		near("оценка", w.Probability(fs[i], cal), want.Scores[i])
	}
	for i, kn := range [][2]float64{{0, 0}, {3, 10}, {7, 7}, {1.5, 4.2}} {
		s := Share(kn[0], kn[1])
		near("доля прочтения L", s.L, want.Share[i][0])
		near("доля прочтения U", s.U, want.Share[i][1])
	}
	for i, kn := range [][2]float64{{0, 0}, {270, 300}, {3, 3}} {
		r := Reliability(kn[0], kn[1], Prior{8, 8})
		near("надёжность L", r.L, want.Reliability[i][0])
		near("надёжность U", r.U, want.Reliability[i][1])
	}
	if _, fitted := FitWeights(fs[:10], ok[:10]); fitted || want.Few != nil {
		t.Fatal("по десяти исходам веса пересчитаны")
	}
}
