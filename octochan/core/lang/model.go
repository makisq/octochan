package lang

import (
	"fmt"
	"strings"
)

// Сценарий, который пишет своя модель (llm.mode: embedded).
//
// Язык алиасов — формальный, но в нём два канала для естественного языка:
// комментарий и строковый литерал (print("…")). Своя модель говорит только
// на ℒ, поэтому в её сценарии комментариев нет, а строка — только имя, путь
// или значение, которое можно проверить: оно есть в задаче, его знает
// ochan, или это значение с единицей («8GB», «30%»). Что допустимо, решает
// known: его собирает ochan по задаче и своим объектам.

// ModelText — что мешает принять сценарий своей модели: комментарии и
// строки, в которых не проверяемое имя или значение. Пусто — сценарий
// принят.
func ModelText(file, src string, known func(text string) bool) []error {
	var out []error
	line, col := 1, 0
	runes := []rune(src)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		col++
		switch {
		case r == '\n':
			line, col = line+1, 0
		case r == '#':
			out = append(out, fmt.Errorf("%s:%d:%d: комментарий в сценарии своей модели: комментарий — естественный язык, "+
				"модель пишет только код и имена из задачи", file, line, col))
			for i+1 < len(runes) && runes[i+1] != '\n' {
				i++
			}
		case r == '"' || r == '\'':
			start, startCol := i, col
			var b strings.Builder
			for i++; i < len(runes) && runes[i] != r && runes[i] != '\n'; i++ {
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
				}
				b.WriteRune(runes[i])
			}
			col += i - start
			text := b.String()
			if text != "" && !known(text) {
				out = append(out, fmt.Errorf("%s:%d:%d: строка %q в сценарии своей модели: в строке — только имя, путь "+
					"или значение из задачи и объектов ochan; текст в ней был бы естественным языком", file, line, startCol, text))
			}
		}
	}
	return out
}
