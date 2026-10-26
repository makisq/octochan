// Command logogen собирает ASCII-логотип для приветствия оболочки.
//
// Картинка переводится в символы не по яркости, а по форме: для каждой
// клетки рисунка подбирается символ, у которого закрашенные места внутри
// знакоместа ближе всего к закрашенным местам клетки (формы символов — в
// glyphs.go). Так получаются контуры в духе neofetch — d$$', Y$$., ,g$$P",
// — а не «мыло» из точек и решёток.
//
// Надпись рисуется отдельным ASCII-шрифтом (wordmark.go): при ширине
// эмблемы в полсотни символов буквам досталось бы меньше четырёх строк, и
// они бы не читались.
//
//	go run ./tools/logogen -in docs/assets/logo.png -out cmd/logo_art.go
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"image/png"
	"math"
	"os"
	"sort"
	"strings"
)

// Индексы цветов в раскраске: 0 — фон (не рисуется), дальше — палитра
// рисунка от светлого к тёмному.
const (
	colorNone   = 0
	colorLight  = 1
	colorAccent = 2
)

type rgb [3]float64

func (c rgb) dist(o rgb) float64 {
	dr, dg, db := c[0]-o[0], c[1]-o[1], c[2]-o[2]
	return dr*dr + dg*dg + db*db
}

func (c rgb) luma() float64 { return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2] }

// picture — картинка, где каждый пиксель отнесён к фону или к цвету палитры.
type picture struct {
	w, h    int
	class   []uint8
	palette [3]rgb
}

type options struct {
	cols   int     // ширина эмблемы в символах
	aspect float64 // высота знакоместа к ширине
	ink    float64 // во сколько раз растянуть заполненность символа
	fill   byte    // символ сплошной заливки
	gap    int     // минимальный разрыв между частями рисунка, пиксели
	glyphs string  // символы, из которых собирается эмблема
}

// defaultGlyphs — набор в духе neofetch. Из всех печатных символов выбор
// по форме тянет в рисунок Q, K, R, %, и картинка превращается в текст;
// узкий набор из знаков-контуров (d b P Y g . , ' ") и одной заливки даёт
// чистые линии.
const defaultGlyphs = ".,:;'`\"-_^~/\\|()dbqpPYgJLo"

// accentShare — доля акцентного цвета в клетке, с которой клетка
// красится акцентом.
const accentShare = 0.4

// defaultOptions — параметры, с которыми собран логотип в cmd/logo_art.go.
// Тест сверяет закоммиченный рисунок с выводом генератора именно на них.
func defaultOptions() options {
	return options{cols: 56, aspect: 2.0, ink: 1.8, fill: '$', gap: 15, glyphs: defaultGlyphs}
}

func main() {
	in := flag.String("in", "", "PNG логотипа")
	out := flag.String("out", "", "Go-файл с готовым рисунком")
	preview := flag.String("preview", "", "дополнительно записать рисунок и раскраску текстом")
	opts := defaultOptions()
	flag.IntVar(&opts.cols, "cols", opts.cols, "ширина эмблемы в символах")
	flag.Float64Var(&opts.aspect, "aspect", opts.aspect, "отношение высоты знакоместа к ширине")
	flag.Float64Var(&opts.ink, "ink", opts.ink, "растяжение заполненности символов")
	fill := flag.String("fill", string(opts.fill), "символ сплошной заливки")
	flag.StringVar(&opts.glyphs, "glyphs", opts.glyphs, "символы, из которых собирается эмблема")
	flag.IntVar(&opts.gap, "gap", opts.gap, "минимальный разрыв между частями рисунка, пиксели")
	flag.Parse()

	if *in == "" || *out == "" || len(*fill) != 1 {
		fmt.Fprintln(os.Stderr, "использование: logogen -in logo.png -out logo_art.go")
		os.Exit(2)
	}
	opts.fill = (*fill)[0]

	if err := run(*in, *out, *preview, opts); err != nil {
		fmt.Fprintln(os.Stderr, "logogen:", err)
		os.Exit(1)
	}
}

func run(in, out, preview string, opts options) error {
	src, drawn, err := generate(in, opts)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, src, 0o644); err != nil {
		return err
	}
	if preview != "" {
		var b strings.Builder
		for i := range drawn.lines {
			b.WriteString(drawn.lines[i] + "\n" + drawn.colors[i] + "\n")
		}
		if err := os.WriteFile(preview, []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// generate собирает исходник logo_art.go из картинки.
func generate(in string, opts options) ([]byte, *art, error) {
	pic, err := load(in)
	if err != nil {
		return nil, nil, err
	}
	a, err := build(pic, opts)
	if err != nil {
		return nil, nil, err
	}
	src, err := render(a, pic.palette)
	return src, a, err
}

// load читает PNG и раскладывает пиксели по фону и двум цветам рисунка.
//
// Фон — цвет левого верхнего угла. Палитра рисунка выводится из самой
// картинки (два кластера по k-средним), а не задаётся руками: перерисовали
// логотип — генератор подстроится сам.
func load(path string) (*picture, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	pixels := make([]rgb, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			pixels[y*w+x] = rgb{float64(r >> 8), float64(g >> 8), float64(bl >> 8)}
		}
	}

	bg := pixels[0]
	var fg []rgb
	for i := 0; i < len(pixels); i += 3 {
		if pixels[i].dist(bg) > 60*60 {
			fg = append(fg, pixels[i])
		}
	}
	if len(fg) == 0 {
		return nil, errors.New("на картинке нет ничего, кроме фона")
	}

	light, accent := kmeans2(fg)
	pic := &picture{w: w, h: h, class: make([]uint8, w*h), palette: [3]rgb{bg, light, accent}}
	for i, p := range pixels {
		best, bestDist := uint8(colorNone), p.dist(bg)
		for k := colorLight; k <= colorAccent; k++ {
			if d := p.dist(pic.palette[k]); d < bestDist {
				best, bestDist = uint8(k), d
			}
		}
		pic.class[i] = best
	}
	return pic, nil
}

// kmeans2 делит цвета рисунка на два кластера и возвращает их центры:
// сначала светлый, потом второй.
func kmeans2(points []rgb) (rgb, rgb) {
	a, b := points[0], points[0]
	for _, p := range points {
		if p.luma() > a.luma() {
			a = p
		}
		// Второе начальное приближение — самый насыщенный цвет.
		if sat(p) > sat(b) {
			b = p
		}
	}
	for iter := 0; iter < 20; iter++ {
		var sa, sb rgb
		var na, nb float64
		for _, p := range points {
			if p.dist(a) <= p.dist(b) {
				sa[0], sa[1], sa[2], na = sa[0]+p[0], sa[1]+p[1], sa[2]+p[2], na+1
			} else {
				sb[0], sb[1], sb[2], nb = sb[0]+p[0], sb[1]+p[1], sb[2]+p[2], nb+1
			}
		}
		if na > 0 {
			a = rgb{sa[0] / na, sa[1] / na, sa[2] / na}
		}
		if nb > 0 {
			b = rgb{sb[0] / nb, sb[1] / nb, sb[2] / nb}
		}
	}
	if b.luma() > a.luma() {
		a, b = b, a
	}
	return a, b
}

func sat(c rgb) float64 {
	hi := math.Max(c[0], math.Max(c[1], c[2]))
	lo := math.Min(c[0], math.Min(c[1], c[2]))
	return hi - lo
}

// band — горизонтальная полоса рисунка: эмблема, надпись или черта.
type band struct{ top, bottom, left, right int }

func (b band) width() int  { return b.right - b.left + 1 }
func (b band) height() int { return b.bottom - b.top + 1 }

// bands делит рисунок на части по пустым полосам между ними. Разрыв
// меньше minGap частей не разделяет: это просвет внутри одной части.
func (p *picture) bands(minGap int) []band {
	var out []band
	last := 0
	for y := 0; y < p.h; y++ {
		if !p.rowHasInk(y) {
			continue
		}
		if n := len(out); n > 0 && y-last <= minGap {
			out[n-1].bottom = y
		} else {
			out = append(out, band{top: y, bottom: y})
		}
		last = y
	}

	for i := range out {
		out[i].left, out[i].right = p.w, -1
		for y := out[i].top; y <= out[i].bottom; y++ {
			for x := 0; x < p.w; x++ {
				if p.class[y*p.w+x] != colorNone {
					out[i].left = min(out[i].left, x)
					out[i].right = max(out[i].right, x)
				}
			}
		}
	}
	return out
}

func (p *picture) rowHasInk(y int) bool {
	for x := 0; x < p.w; x++ {
		if p.class[y*p.w+x] != colorNone {
			return true
		}
	}
	return false
}

// art — готовый рисунок: строки символов и раскраска той же длины.
type art struct {
	lines  []string
	colors []string
}

func build(pic *picture, opts options) (*art, error) {
	parts := pic.bands(opts.gap)
	if len(parts) != 3 {
		return nil, fmt.Errorf("ожидались три части — эмблема, надпись, черта; найдено %d: %+v", len(parts), parts)
	}
	emblem, word, line := parts[0], parts[1], parts[2]

	wordmark := drawWordmark("OCTOCHAN", opts.fill)

	result := &art{}
	result.add(drawEmblem(pic, emblem, opts))
	result.blank()
	result.add(wordmark)
	result.blank()

	// Черта под надписью сохраняет пропорцию картинки.
	wordCols := len(wordmark.lines[0])
	result.add(drawUnderline(int(math.Round(float64(line.width()) / float64(word.width()) * float64(wordCols)))))

	result.center()
	return result, nil
}

func (a *art) add(part *art) {
	a.lines = append(a.lines, part.lines...)
	a.colors = append(a.colors, part.colors...)
}

func (a *art) blank() {
	a.lines = append(a.lines, "")
	a.colors = append(a.colors, "")
}

// center выравнивает части по общей оси, затем срезает лишние поля слева
// и пробелы справа.
func (a *art) center() {
	width := 0
	for _, l := range a.lines {
		width = max(width, len(strings.TrimRight(l, " ")))
	}
	for i, l := range a.lines {
		trimmed := strings.TrimRight(l, " ")
		lead := len(trimmed) - len(strings.TrimLeft(trimmed, " "))
		body := trimmed[lead:]
		pad := (width - len(body)) / 2
		a.lines[i] = strings.Repeat(" ", pad) + body
		a.colors[i] = strings.Repeat(" ", pad) + a.colors[i][lead:lead+len(body)]
	}

	margin := math.MaxInt
	for _, l := range a.lines {
		if strings.TrimSpace(l) != "" {
			margin = min(margin, len(l)-len(strings.TrimLeft(l, " ")))
		}
	}
	for i := range a.lines {
		if len(a.lines[i]) >= margin {
			a.lines[i] = a.lines[i][margin:]
			a.colors[i] = a.colors[i][margin:]
		}
	}
}

// drawEmblem переводит эмблему в символы по форме.
func drawEmblem(pic *picture, b band, opts options) *art {
	cw := float64(b.width()) / float64(opts.cols)
	ch := cw * opts.aspect
	rows := int(math.Ceil(float64(b.height()) / ch))
	top := float64(b.top) - (float64(rows)*ch-float64(b.height()))/2

	glyphs := glyphList(opts.glyphs, opts.fill)
	out := &art{}
	for r := 0; r < rows; r++ {
		var line, colors strings.Builder
		for c := 0; c < opts.cols; c++ {
			x0 := float64(b.left) + float64(c)*cw
			y0 := top + float64(r)*ch
			cov, color := pic.sample(x0, y0, cw, ch)
			g := pick(cov, glyphs, opts)
			line.WriteByte(g)
			if g == ' ' {
				colors.WriteByte(' ')
			} else {
				colors.WriteByte(byte('0' + color))
			}
		}
		out.lines = append(out.lines, line.String())
		out.colors = append(out.colors, colors.String())
	}
	return out
}

// sample считает заполненность ячеек сетки glyphCols×glyphRows внутри
// клетки и преобладающий в ней цвет.
func (p *picture) sample(x0, y0, w, h float64) ([glyphCols * glyphRows]float64, int) {
	var cov [glyphCols * glyphRows]float64
	var counts [3]int
	for sy := 0; sy < glyphRows; sy++ {
		for sx := 0; sx < glyphCols; sx++ {
			ax, bx := x0+float64(sx)*w/glyphCols, x0+float64(sx+1)*w/glyphCols
			ay, by := y0+float64(sy)*h/glyphRows, y0+float64(sy+1)*h/glyphRows
			filled, total := 0, 0
			for y := int(math.Floor(ay)); y < int(math.Ceil(by)); y++ {
				for x := int(math.Floor(ax)); x < int(math.Ceil(bx)); x++ {
					total++
					if x < 0 || y < 0 || x >= p.w || y >= p.h {
						continue
					}
					k := p.class[y*p.w+x]
					counts[k]++
					if k != colorNone {
						filled++
					}
				}
			}
			if total > 0 {
				cov[sy*glyphCols+sx] = float64(filled) / float64(total)
			}
		}
	}
	// Акцентный цвет занимает мелкие, но главные детали — глаза, кольцо,
	// края щупалец. По простому большинству они тонули бы в светлом, поэтому
	// акценту хватает заметной доли.
	color := colorLight
	if fg := counts[colorLight] + counts[colorAccent]; fg > 0 && float64(counts[colorAccent]) >= accentShare*float64(fg) {
		color = colorAccent
	}
	return cov, color
}

// glyphList возвращает символы-кандидаты: заданный набор и заливку.
func glyphList(set string, fill byte) []byte {
	list := []byte{fill}
	for i := 0; i < len(set); i++ {
		if _, ok := glyphShapes[set[i]]; ok && set[i] != ' ' && set[i] != fill {
			list = append(list, set[i])
		}
	}
	// Порядок фиксирован: при равенстве оценок выбор не должен зависеть
	// от порядка перечисления, иначе рисунок менялся бы от правки к правке.
	sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
	return list
}

// pick выбирает символ, форма которого ближе всего к клетке.
func pick(cov [glyphCols * glyphRows]float64, glyphs []byte, opts options) byte {
	mean, low := 0.0, 1.0
	for _, v := range cov {
		mean += v
		low = math.Min(low, v)
	}
	mean /= float64(len(cov))

	switch {
	case mean < 0.06:
		return ' '
	case low > 0.85:
		return opts.fill
	}

	best, bestScore := byte(' '), squared(cov, nil, 0)
	for _, g := range glyphs {
		shape := glyphShapes[g]
		if score := squared(cov, shape[:], opts.ink); score < bestScore {
			best, bestScore = g, score
		}
	}
	return best
}

func squared(cov [glyphCols * glyphRows]float64, shape []float64, ink float64) float64 {
	sum := 0.0
	for i, v := range cov {
		s := 0.0
		if shape != nil {
			s = math.Min(1, shape[i]*ink)
		}
		d := v - s
		sum += d * d
	}
	return sum
}

// render записывает рисунок Go-файлом для пакета cmd.
func render(a *art, palette [3]rgb) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("// Code generated by tools/logogen; DO NOT EDIT.\n\n")
	b.WriteString("package cmd\n\n")
	b.WriteString("// logoPalette — цвета логотипа; индекс — цифра в logoColors.\n")
	b.WriteString("var logoPalette = [...][3]uint8{\n")
	names := []string{"фон, не рисуется", "светлый", "акцент"}
	for i, c := range palette {
		fmt.Fprintf(&b, "\t{%d, %d, %d}, // %d — %s\n", round(c[0]), round(c[1]), round(c[2]), i, names[i])
	}
	b.WriteString("}\n\n")
	b.WriteString("// logoLines — рисунок: только печатные символы ASCII.\n")
	b.WriteString("var logoLines = []string{\n")
	for _, l := range a.lines {
		fmt.Fprintf(&b, "\t%q,\n", l)
	}
	b.WriteString("}\n\n")
	b.WriteString("// logoColors — цвет каждого символа logoLines: цифра — индекс в logoPalette.\n")
	b.WriteString("var logoColors = []string{\n")
	for _, c := range a.colors {
		fmt.Fprintf(&b, "\t%q,\n", c)
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}

func round(v float64) int { return int(math.Round(v)) }
