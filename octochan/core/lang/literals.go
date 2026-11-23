package lang

import (
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Литералы сценария и происхождение значений плана.
//
// Значения языка — простые типы, метку источника к ним не прицепить.
// Поэтому происхождение выясняется опытом: план строится второй раз, а
// числа, которые модель вписала в сценарий, сдвигаются на единицу.
// Значение плана, которое от этого поменялось, зависит от решения модели —
// напрямую или через вычисление (100 * 2). Значение из предпосылок —
// файла, живого источника, инвентаря — остаётся прежним.

// literalWatch — наблюдение за литералами одного файла.
type literalWatch struct {
	file    string
	perturb bool
	mu      sync.Mutex
	seen    map[string]bool
	order   []string
}

// WatchLiterals — запоминать литералы файла file, дошедшие до выполнения;
// с perturb — ещё и сдвигать числа в них (Perturb).
func (in *Interpreter) WatchLiterals(file string, perturb bool) {
	in.lits = &literalWatch{file: file, perturb: perturb, seen: map[string]bool{}}
}

// Literals — литералы сценария, дошедшие до выполнения, по порядку.
func (in *Interpreter) Literals() []string {
	if in.lits == nil {
		return nil
	}
	in.lits.mu.Lock()
	defer in.lits.mu.Unlock()
	return append([]string(nil), in.lits.order...)
}

func (w *literalWatch) see(v Value) Value {
	switch v.(type) {
	case StringValue, IntValue, FloatValue:
	default:
		return v
	}
	text := v.Text()
	w.mu.Lock()
	if text != "" && !w.seen[text] {
		w.seen[text] = true
		w.order = append(w.order, text)
	}
	w.mu.Unlock()
	if w.perturb {
		return Perturb(v)
	}
	return v
}

// Perturb сдвигает число на единицу: 200 → 201, "16MB" → "17MB", 0.5 →
// 1.5. Не число — как есть.
func Perturb(v Value) Value {
	switch x := v.(type) {
	case IntValue:
		return x + 1
	case FloatValue:
		return x + 1
	case StringValue:
		if shifted, ok := perturbText(string(x)); ok {
			return StringValue(shifted)
		}
	}
	return v
}

// perturbText — число с необязательной единицей в начале строки: 200,
// 16MB, 30 s, 0.5. Остальное — не число.
func perturbText(s string) (string, bool) {
	end := 0
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.') {
		end++
	}
	if end == 0 {
		return "", false
	}
	unit := s[end:]
	for _, r := range strings.TrimSpace(unit) {
		if !unicode.IsLetter(r) && r != '%' && r != '/' {
			return "", false
		}
	}
	if n, err := strconv.ParseInt(s[:end], 10, 64); err == nil {
		return strconv.FormatInt(n+1, 10) + unit, true
	}
	f, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatFloat(f+1, 'f', -1, 64) + unit, true
}
