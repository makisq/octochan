package cmd

import (
	"fmt"
	"strings"
)

// Построчное сравнение ochan diff — для всего, что не конфиг: кратчайший
// список правок по Майерсу (O((N+M)·D)) и вывод в формате diff -u.

// maxDiffEdits — сколько различий построчное сравнение ещё считает: память
// алгоритма растёт с их числом, а сравнивать совсем разные файлы построчно
// незачем.
const maxDiffEdits = 10000

// configLike — текст похож на конфиг: почти каждая значимая строка —
// [секция] или параметр «имя = значение» либо «имя: значение» с именем без
// пробелов (include postgresql.conf — тоже). Журнал или произвольный текст
// по смыслу не сравнить: разбор конфига пропустил бы его строки, и два
// разных журнала вышли бы одинаковыми.
func configLike(content string) bool {
	params, other := 0, 0
	for _, raw := range strings.Split(strings.TrimPrefix(content, "\uFEFF"), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "", strings.HasPrefix(line, "#"), strings.HasPrefix(line, ";"), strings.HasPrefix(line, "//"),
			strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"), strings.HasPrefix(line, "include"):
			continue
		}
		if i := strings.IndexAny(line, "=:"); i > 0 && !strings.ContainsAny(strings.TrimSpace(line[:i]), " \t") {
			params++
			continue
		}
		other++
	}
	return params > 0 && other*10 <= params+other
}

// splitLines — строки текста без последнего пустого «хвоста».
func splitLines(text string) []string {
	text = strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// lineEdit — строка сравнения: ' ' — общая, '-' — только в первом файле,
// '+' — только во втором; a и b — сколько строк каждого файла было до неё.
type lineEdit struct {
	op   byte
	text string
	a, b int
}

// diffLines — правки, превращающие a в b.
func diffLines(a, b []string) ([]lineEdit, error) {
	n, m := len(a), len(b)
	limit := n + m
	if limit > maxDiffEdits {
		limit = maxDiffEdits
	}
	off := limit + 1
	v := make([]int, 2*limit+3)
	var trace [][]int
	found := false
	for d := 0; d <= limit && !found; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("различий больше %d строк: сравните файлы по частям", maxDiffEdits)
	}

	var rev []lineEdit
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[off+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x, y = x-1, y-1
			rev = append(rev, lineEdit{op: ' ', text: a[x]})
		}
		if d > 0 {
			if x == prevX {
				rev = append(rev, lineEdit{op: '+', text: b[prevY]})
			} else {
				rev = append(rev, lineEdit{op: '-', text: a[prevX]})
			}
		}
		x, y = prevX, prevY
	}

	edits := make([]lineEdit, len(rev))
	ai, bi := 0, 0
	for i := range rev {
		e := rev[len(rev)-1-i]
		e.a, e.b = ai, bi
		switch e.op {
		case ' ':
			ai, bi = ai+1, bi+1
		case '-':
			ai++
		case '+':
			bi++
		}
		edits[i] = e
	}
	return edits, nil
}

// unified — правки в формате diff -u с context строками вокруг изменений;
// изменений нет — пусто.
func unified(nameA, nameB string, edits []lineEdit, context int) string {
	var changes []int
	for i, e := range edits {
		if e.op != ' ' {
			changes = append(changes, i)
		}
	}
	if len(changes) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", nameA, nameB)
	for i := 0; i < len(changes); {
		start := max(0, changes[i]-context)
		end := changes[i]
		// Изменения ближе 2·context строк друг к другу — один кусок.
		for i < len(changes) && changes[i] <= end+2*context+1 {
			end = changes[i]
			i++
		}
		end = min(len(edits)-1, end+context)
		hunk := edits[start : end+1]
		lenA, lenB := 0, 0
		for _, e := range hunk {
			if e.op != '+' {
				lenA++
			}
			if e.op != '-' {
				lenB++
			}
		}
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", hunkRange(hunk[0].a, lenA), hunkRange(hunk[0].b, lenB))
		for _, e := range hunk {
			b.WriteByte(e.op)
			b.WriteString(e.text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// hunkRange — «начало,длина» куска: строки с 1, у пустого — строка перед ним.
func hunkRange(before, length int) string {
	start := before + 1
	if length == 0 {
		start = before
	}
	if length == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, length)
}
