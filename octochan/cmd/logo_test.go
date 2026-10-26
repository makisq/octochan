package cmd

import (
	"regexp"
	"strings"
	"testing"
)

func TestLogoIsPureASCII(t *testing.T) {
	// Логотип — только печатные символы ASCII: ни блоков Unicode, ни
	// графических протоколов терминала. Цвет задаётся отдельно.
	if len(logoLines) != len(logoColors) {
		t.Fatalf("строк рисунка %d, строк раскраски %d", len(logoLines), len(logoColors))
	}
	for i, line := range logoLines {
		colors := logoColors[i]
		if len(colors) != len(line) {
			t.Fatalf("строка %d: рисунок %d символов, раскраска %d", i, len(line), len(colors))
		}
		for j := 0; j < len(line); j++ {
			if line[j] < 0x20 || line[j] > 0x7e {
				t.Fatalf("строка %d, позиция %d: не печатный ASCII %q", i, j, line[j])
			}
			blank := line[j] == ' '
			if blank != (colors[j] == ' ') {
				t.Fatalf("строка %d, позиция %d: символ %q и цвет %q не согласованы", i, j, line[j], colors[j])
			}
			if !blank && (colors[j] < '1' || int(colors[j]-'0') >= len(logoPalette)) {
				t.Fatalf("строка %d, позиция %d: цвета %q нет в палитре", i, j, colors[j])
			}
		}
	}
}

func TestLogoFitsStandardTerminal(t *testing.T) {
	// 80 колонок — ширина, которую надо считать доступной всегда.
	if w := logoWidth(); w > 80 {
		t.Errorf("логотип шириной %d не помещается в 80 колонок", w)
	}
	if !strings.Contains(strings.Join(logoLines, "\n"), "$$") {
		t.Error("в рисунке нет заливки — генератор отработал не так")
	}
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestRenderLogoDepths(t *testing.T) {
	plain := strings.Join(logoLines, "\n") + "\n"

	if got := renderLogo(colorsNone); got != plain {
		t.Error("без цвета вывод отличается от рисунка")
	}

	for depth, marker := range map[colorDepth]string{
		colorsTrue: "\x1b[38;2;",
		colors256:  "\x1b[38;5;",
		colors16:   "\x1b[9",
	} {
		got := renderLogo(depth)
		if !strings.Contains(got, marker) {
			t.Errorf("глубина %d: нет кода цвета %q", depth, marker)
		}
		if stripped := ansi.ReplaceAllString(got, ""); stripped != plain {
			t.Errorf("глубина %d: без кодов цвета рисунок искажён", depth)
		}
		// Цвет не должен перетекать в следующую строку и в приглашение.
		for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			if strings.Contains(line, "\x1b[3") && !strings.HasSuffix(line, "\x1b[0m") {
				t.Errorf("глубина %d, строка %d: цвет не сброшен в конце", depth, i)
			}
		}
	}
}

func TestDetectColorDepth(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		goos string
		want colorDepth
	}{
		{"NO_COLOR сильнее всего", map[string]string{"NO_COLOR": "1", "COLORTERM": "truecolor"}, "linux", colorsNone},
		{"dumb", map[string]string{"TERM": "dumb"}, "linux", colorsNone},
		{"truecolor", map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"}, "linux", colorsTrue},
		{"24bit", map[string]string{"COLORTERM": "24bit"}, "darwin", colorsTrue},
		{"256 цветов", map[string]string{"TERM": "xterm-256color"}, "linux", colors256},
		{"базовый терминал", map[string]string{"TERM": "xterm"}, "linux", colors16},
		{"консоль Windows", map[string]string{}, "windows", colorsTrue},
		{"неизвестно", map[string]string{}, "linux", colorsNone},
	}
	for _, c := range cases {
		getenv := func(key string) string { return c.env[key] }
		if got := detectColorDepth(getenv, c.goos); got != c.want {
			t.Errorf("%s: %d, ожидалось %d", c.name, got, c.want)
		}
	}
}

func TestShouldShowLogo(t *testing.T) {
	w := logoWidth()
	cases := []struct {
		name              string
		enabled, terminal bool
		columns           int
		want              bool
	}{
		{"терминал нужной ширины", true, true, w, true},
		{"выключен настройкой или флагом", false, true, 200, false},
		{"вывод не в терминал: CI, файл, конвейер", true, false, 200, false},
		{"узкий терминал — строки бы перенеслись", true, true, w - 1, false},
	}
	for _, c := range cases {
		if got := shouldShowLogo(c.enabled, c.terminal, c.columns); got != c.want {
			t.Errorf("%s: %v, ожидалось %v", c.name, got, c.want)
		}
	}
}

func TestNearestColors(t *testing.T) {
	if got := nearest256([3]uint8{255, 0, 0}); got != 196 {
		t.Errorf("красный → %d, ожидался 196", got)
	}
	if got := nearest256([3]uint8{128, 128, 128}); got < 232 {
		t.Errorf("серый ушёл в куб: %d", got)
	}
	if got := nearest16(logoPalette[1]); got != 97 {
		t.Errorf("светлый цвет логотипа → %d, ожидался ярко-белый 97", got)
	}
	if got := nearest16(logoPalette[2]); got != 94 {
		t.Errorf("фиолетовый логотипа → %d, ожидался ярко-синий 94", got)
	}
}
