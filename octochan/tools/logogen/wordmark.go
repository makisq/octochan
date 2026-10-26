package main

import "strings"

// wordmarkFont — буквы надписи высотой в пять строк. «#» заменяется
// символом заливки эмблемы, чтобы надпись и рисунок были одной фактуры.
//
// Начертание повторяет шрифт логотипа: ровная толщина штриха, скруглённые
// углы у O, C и A, прямые — у T, H и N.
var wordmarkFont = map[rune][]string{
	'O': {
		".d###b.",
		"##   ##",
		"##   ##",
		"##   ##",
		"`Y###P'",
	},
	'C': {
		".d#####",
		"##     ",
		"##     ",
		"##     ",
		"`Y#####",
	},
	'T': {
		"######",
		"  ##  ",
		"  ##  ",
		"  ##  ",
		"  ##  ",
	},
	'H': {
		"##   ##",
		"##   ##",
		"#######",
		"##   ##",
		"##   ##",
	},
	'A': {
		".d###b.",
		"##   ##",
		"#######",
		"##   ##",
		"##   ##",
	},
	'N': {
		"##b  ##",
		"###b ##",
		"## Yb##",
		"##  Y##",
		"##   ##",
	},
}

// drawWordmark набирает слово шрифтом wordmarkFont.
func drawWordmark(text string, fill byte) *art {
	const height = 5
	out := &art{lines: make([]string, height), colors: make([]string, height)}
	for i, r := range text {
		glyph, ok := wordmarkFont[r]
		if !ok {
			panic("в шрифте надписи нет буквы " + string(r))
		}
		for row := 0; row < height; row++ {
			if i > 0 {
				out.lines[row] += " "
			}
			out.lines[row] += strings.ReplaceAll(glyph[row], "#", string(fill))
		}
	}
	for row := range out.lines {
		out.colors[row] = paint(out.lines[row], colorLight)
	}
	return out
}

// drawUnderline рисует черту под надписью: две линии и точка посередине.
func drawUnderline(width int) *art {
	const dot = "  @  "
	side := (width - len(dot)) / 2
	line := strings.Repeat("-", side) + dot + strings.Repeat("-", side)
	return &art{lines: []string{line}, colors: []string{paint(line, colorAccent)}}
}

// paint раскрашивает все непробельные символы строки одним цветом.
func paint(line string, color int) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == ' ' {
			b.WriteByte(' ')
		} else {
			b.WriteByte(byte('0' + color))
		}
	}
	return b.String()
}
