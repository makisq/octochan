package cmd

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/viper"
	"golang.org/x/term"
)

// Рисунок собирается из docs/assets/logo.png генератором; руками его не
// правят. После смены картинки:
//
//go:generate go run ../tools/logogen -in ../docs/assets/logo.png -out logo_art.go

// colorDepth — сколько цветов умеет терминал.
type colorDepth int

const (
	colorsNone colorDepth = iota // без цвета: NO_COLOR, TERM=dumb, неизвестный терминал
	colors16                     // базовые 16 цветов
	colors256                    // палитра xterm на 256 цветов
	colorsTrue                   // 24-битный цвет
)

// detectColorDepth определяет глубину цвета по переменным окружения.
//
// NO_COLOR (no-color.org) отключает цвет при любом значении, кроме пустого.
// Windows проверяется отдельно: консоль Windows 10 и Windows Terminal
// понимают 24-битный цвет, когда включён разбор ANSI-последовательностей,
// а переменных вроде COLORTERM не выставляют.
func detectColorDepth(getenv func(string) string, goos string) colorDepth {
	if getenv("NO_COLOR") != "" {
		return colorsNone
	}
	termName := getenv("TERM")
	if termName == "dumb" {
		return colorsNone
	}
	switch strings.ToLower(getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return colorsTrue
	}
	switch {
	case goos == "windows":
		return colorsTrue
	case strings.Contains(termName, "256color"):
		return colors256
	case termName != "":
		return colors16
	}
	return colorsNone
}

// logoWidth — ширина рисунка в символах.
func logoWidth() int {
	width := 0
	for _, line := range logoLines {
		width = max(width, len(line))
	}
	return width
}

// shouldShowLogo решает, показывать ли логотип при входе в оболочку.
//
// Только на терминале: в журнале CI или в файле рисунок — мусор. И только
// если он помещается по ширине: перенесённые строки разваливают картинку.
func shouldShowLogo(enabled, isTerminal bool, columns int) bool {
	return enabled && isTerminal && columns >= logoWidth()
}

// renderLogo собирает рисунок с цветом нужной глубины.
func renderLogo(depth colorDepth) string {
	var b strings.Builder
	for i, line := range logoLines {
		colors := logoColors[i]
		current := byte(' ')
		for j := 0; j < len(line); j++ {
			if c := colors[j]; c != ' ' && c != current && depth != colorsNone {
				b.WriteString(colorCode(depth, logoPalette[c-'0']))
				current = c
			}
			b.WriteByte(line[j])
		}
		// Сброс в конце каждой строки: цвет не должен перетечь в следующую
		// строку и тем более в приглашение, если вывод прервут на середине.
		if current != ' ' {
			b.WriteString("\x1b[0m")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func colorCode(depth colorDepth, c [3]uint8) string {
	switch depth {
	case colorsTrue:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c[0], c[1], c[2])
	case colors256:
		return fmt.Sprintf("\x1b[38;5;%dm", nearest256(c))
	default:
		return fmt.Sprintf("\x1b[%dm", nearest16(c))
	}
}

// nearest256 подбирает ближайший цвет палитры xterm: куб 6×6×6 или серый.
func nearest256(c [3]uint8) int {
	levels := [6]int{0, 95, 135, 175, 215, 255}
	index := func(v uint8) int {
		best := 0
		for i, l := range levels {
			if abs(int(v)-l) < abs(int(v)-levels[best]) {
				best = i
			}
		}
		return best
	}
	r, g, b := index(c[0]), index(c[1]), index(c[2])
	cube := 16 + 36*r + 6*g + b
	cubeDist := dist3([3]int{levels[r], levels[g], levels[b]}, c)

	gray := (int(c[0]) + int(c[1]) + int(c[2])) / 3
	step := min(23, max(0, (gray-8+5)/10))
	grayLevel := 8 + 10*step
	if dist3([3]int{grayLevel, grayLevel, grayLevel}, c) < cubeDist {
		return 232 + step
	}
	return cube
}

// nearest16 подбирает ближайший из 16 базовых цветов и возвращает код SGR.
func nearest16(c [3]uint8) int {
	// Эталон — палитра Campbell из Windows Terminal: она ближе к нынешним
	// умолчаниям, чем цвета xterm. В xterm белый (37) и ярко-белый (97)
	// оказывались на равном расстоянии от светлого цвета логотипа, а в
	// большинстве тем 37 заметно серый.
	palette := []struct {
		code int
		rgb  [3]int
	}{
		{30, [3]int{12, 12, 12}}, {31, [3]int{197, 15, 31}}, {32, [3]int{19, 161, 14}}, {33, [3]int{193, 156, 0}},
		{34, [3]int{0, 55, 218}}, {35, [3]int{136, 23, 152}}, {36, [3]int{58, 150, 221}}, {37, [3]int{204, 204, 204}},
		{90, [3]int{118, 118, 118}}, {91, [3]int{231, 72, 86}}, {92, [3]int{22, 198, 12}}, {93, [3]int{249, 241, 165}},
		{94, [3]int{59, 120, 255}}, {95, [3]int{180, 0, 158}}, {96, [3]int{97, 214, 214}}, {97, [3]int{242, 242, 242}},
	}
	best := palette[0]
	for _, p := range palette {
		if dist3(p.rgb, c) < dist3(best.rgb, c) {
			best = p
		}
	}
	return best.code
}

func dist3(a [3]int, c [3]uint8) int {
	dr, dg, db := a[0]-int(c[0]), a[1]-int(c[1]), a[2]-int(c[2])
	return dr*dr + dg*dg + db*db
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// shellNoLogo — флаг ochan shell --no-logo.
var shellNoLogo bool

// printShellLogo выводит логотип при входе в оболочку, если терминал это
// позволяет. Ничего не выводит в остальных случаях — и никогда не мешает
// входу в оболочку.
func printShellLogo(out *os.File) {
	fd := int(out.Fd())
	isTerminal := term.IsTerminal(fd)
	columns := 80
	if w, _, err := term.GetSize(fd); err == nil {
		columns = w
	}
	if !shouldShowLogo(viper.GetBool("shell.logo") && !shellNoLogo, isTerminal, columns) {
		return
	}

	depth := detectColorDepth(os.Getenv, runtime.GOOS)
	if depth != colorsNone && !enableVirtualTerminal(out) {
		depth = colorsNone
	}
	writeLogo(out, depth)
}

func writeLogo(w io.Writer, depth colorDepth) {
	fmt.Fprint(w, renderLogo(depth))
	fmt.Fprintln(w)
}
