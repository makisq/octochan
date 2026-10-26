package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestGeneratedArtUpToDate(t *testing.T) {
	// Рисунок в cmd/logo_art.go должен получаться из картинки генератором,
	// а не правиться руками: иначе после смены логотипа никто не узнает,
	// как его перерисовать.
	src, _, err := generate("../../docs/assets/logo.png", defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("../../cmd/logo_art.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(normalize(src), normalize(committed)) {
		t.Error("cmd/logo_art.go устарел: выполните go generate ./cmd")
	}
}

// normalize убирает разницу в переводах строк: на Windows git может
// выдать файл с CRLF.
func normalize(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }

func TestWordmarkLettersAligned(t *testing.T) {
	for r, rows := range wordmarkFont {
		if len(rows) != 5 {
			t.Errorf("%c: строк %d, ожидалось 5", r, len(rows))
		}
		for i, row := range rows {
			if len(row) != len(rows[0]) {
				t.Errorf("%c: строка %d шириной %d, первая — %d", r, i, len(row), len(rows[0]))
			}
		}
	}

	word := drawWordmark("OCTOCHAN", '$')
	for i, line := range word.lines {
		if len(line) != len(word.lines[0]) || len(word.colors[i]) != len(line) {
			t.Errorf("строка %d надписи не выровнена: %q", i, line)
		}
	}
}

func TestPickExtremes(t *testing.T) {
	opts := defaultOptions()
	glyphs := glyphList(opts.glyphs, opts.fill)

	var empty, full [glyphCols * glyphRows]float64
	for i := range full {
		full[i] = 1
	}
	if g := pick(empty, glyphs, opts); g != ' ' {
		t.Errorf("пустая клетка стала %q", g)
	}
	if g := pick(full, glyphs, opts); g != opts.fill {
		t.Errorf("сплошная клетка стала %q вместо заливки", g)
	}

	// Заполнена только нижняя четверть — нужен символ, прижатый к низу.
	var bottom [glyphCols * glyphRows]float64
	for i := glyphCols * (glyphRows - 2); i < len(bottom); i++ {
		bottom[i] = 1
	}
	if g := pick(bottom, glyphs, opts); !strings.ContainsRune("._,g", rune(g)) {
		t.Errorf("клетка с заполненным низом стала %q", g)
	}
}
