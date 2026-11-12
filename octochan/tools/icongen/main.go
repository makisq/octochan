// Command icongen делает значок программы из логотипа.
//
// Из docs/assets/logo.png берётся эмблема без надписи — квадрат вокруг неё
// на фоне логотипа, со скруглёнными углами — и пишется в двух видах:
//
//   - cmd/icon.png, 256×256: его несёт в себе ochan и кладёт в меню
//     приложений и на рабочий стол (ochan setup --desktop);
//   - rsrc_windows_amd64.syso: ресурс значка для Windows. go build сам
//     прилинковывает файл *.syso рядом с main.go, и у ochan.exe появляется
//     значок — в проводнике, поиске и на ярлыках.
//
// Внешних программ (windres, rsrc) не нужно: файл ресурсов COFF пишется
// здесь же.
//
//	go run ./tools/icongen -in docs/assets/logo.png -png cmd/icon.png -syso rsrc_windows_amd64.syso
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

// Размеры значка в ресурсе Windows: от списка файлов до крупных плиток.
var iconSizes = []int{16, 24, 32, 48, 64, 128, 256}

func main() {
	in := flag.String("in", "docs/assets/logo.png", "логотип")
	pngOut := flag.String("png", "cmd/icon.png", "значок PNG 256×256")
	sysoOut := flag.String("syso", "rsrc_windows_amd64.syso", "ресурс значка для Windows (amd64)")
	icoOut := flag.String("ico", "", "ещё и файл .ico")
	flag.Parse()
	if err := run(*in, *pngOut, *sysoOut, *icoOut); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func run(in, pngOut, sysoOut, icoOut string) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	logo, err := png.Decode(f)
	if err != nil {
		return fmt.Errorf("%s: %w", in, err)
	}
	icon, err := emblem(logo)
	if err != nil {
		return fmt.Errorf("%s: %w", in, err)
	}
	images := map[int]*image.NRGBA{}
	for _, size := range iconSizes {
		images[size] = resize(icon, size)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, images[256]); err != nil {
		return err
	}
	if err := os.WriteFile(pngOut, buf.Bytes(), 0o644); err != nil {
		return err
	}
	entries, err := icoEntries(images)
	if err != nil {
		return err
	}
	if err := os.WriteFile(sysoOut, syso(entries, machineAMD64), 0o644); err != nil {
		return err
	}
	if icoOut != "" {
		if err := os.WriteFile(icoOut, ico(entries), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("%s: 256×256, %d байт\n%s: значки %v, %d байт\n", pngOut, buf.Len(), sysoOut, iconSizes, len(syso(entries, machineAMD64)))
	return nil
}

// emblem — эмблема логотипа без надписи: квадрат вокруг неё на фоне
// логотипа, углы скруглены. Эмблема — первая сверху часть рисунка; от
// надписи её отделяет полоса фона.
func emblem(logo image.Image) (*image.NRGBA, error) {
	b := logo.Bounds()
	background := color.NRGBAModel.Convert(logo.At(b.Min.X, b.Min.Y)).(color.NRGBA)
	ink := func(x, y int) bool {
		c := color.NRGBAModel.Convert(logo.At(x, y)).(color.NRGBA)
		dr, dg, db := int(c.R)-int(background.R), int(c.G)-int(background.G), int(c.B)-int(background.B)
		return dr*dr+dg*dg+db*db > 40*40
	}
	const gap = 12 // строк фона подряд — конец эмблемы
	top, bottom, empty := -1, -1, 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := false
		for x := b.Min.X; x < b.Max.X && !row; x++ {
			row = ink(x, y)
		}
		switch {
		case row && top < 0:
			top, bottom, empty = y, y, 0
		case row:
			bottom, empty = y, 0
		case top >= 0:
			if empty++; empty >= gap {
				y = b.Max.Y
			}
		}
	}
	if top < 0 {
		return nil, errors.New("рисунка нет: один фон")
	}
	left, right := b.Max.X, b.Min.X
	for y := top; y <= bottom; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if ink(x, y) {
				left, right = min(left, x), max(right, x)
			}
		}
	}
	// Квадрат с полями вокруг эмблемы, по её центру.
	side := int(math.Round(float64(max(right-left, bottom-top)+1) * 1.24))
	x0, y0 := (left+right+1)/2-side/2, (top+bottom+1)/2-side/2
	if x0 < b.Min.X || y0 < b.Min.Y || x0+side > b.Max.X || y0+side > b.Max.Y {
		return nil, fmt.Errorf("эмблема %d×%d у края рисунка: квадрат со стороной %d не помещается", right-left+1, bottom-top+1, side)
	}
	out := image.NewNRGBA(image.Rect(0, 0, side, side))
	radius := float64(side) * 0.22
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			// Ниже эмблемы в квадрат попал бы край надписи: там — фон.
			c := background
			if y0+y >= top && y0+y <= bottom {
				c = color.NRGBAModel.Convert(logo.At(x0+x, y0+y)).(color.NRGBA)
			}
			c.A = uint8(math.Round(255 * corner(float64(x)+0.5, float64(y)+0.5, float64(side), radius)))
			out.SetNRGBA(x, y, c)
		}
	}
	return out, nil
}

// corner — непрозрачность точки квадрата со скруглёнными углами: 1 внутри,
// 0 снаружи, на границе — плавно.
func corner(x, y, side, radius float64) float64 {
	dx := math.Max(math.Max(radius-x, x-(side-radius)), 0)
	dy := math.Max(math.Max(radius-y, y-(side-radius)), 0)
	d := radius - math.Hypot(dx, dy)
	return math.Min(math.Max(d+0.5, 0), 1)
}

// resize уменьшает картинку до size×size усреднением по площади: каждый
// пиксель результата — среднее исходных под ним, с учётом прозрачности.
func resize(src *image.NRGBA, size int) *image.NRGBA {
	n := src.Bounds().Dx()
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := float64(n) / float64(size)
	for y := 0; y < size; y++ {
		y0, y1 := float64(y)*scale, float64(y+1)*scale
		for x := 0; x < size; x++ {
			x0, x1 := float64(x)*scale, float64(x+1)*scale
			var r, g, b, a, area float64
			for sy := int(y0); sy < n && float64(sy) < y1; sy++ {
				wy := math.Min(float64(sy+1), y1) - math.Max(float64(sy), y0)
				for sx := int(x0); sx < n && float64(sx) < x1; sx++ {
					w := wy * (math.Min(float64(sx+1), x1) - math.Max(float64(sx), x0))
					c := src.NRGBAAt(sx, sy)
					alpha := float64(c.A) / 255 * w
					r, g, b, a, area = r+float64(c.R)*alpha, g+float64(c.G)*alpha, b+float64(c.B)*alpha, a+alpha, area+w
				}
			}
			if a > 0 {
				dst.SetNRGBA(x, y, color.NRGBA{uint8(math.Round(r / a)), uint8(math.Round(g / a)), uint8(math.Round(b / a)),
					uint8(math.Round(255 * a / area))})
			}
		}
	}
	return dst
}

// icoEntry — картинка значка одного размера, как она лежит в .ico и в
// ресурсе RT_ICON.
type icoEntry struct {
	size int
	data []byte
}

// icoEntries — картинки значка: 256 — PNG, меньшие — DIB с альфа-каналом
// (так их читает и старый код Windows).
func icoEntries(images map[int]*image.NRGBA) ([]icoEntry, error) {
	var out []icoEntry
	for _, size := range iconSizes {
		img := images[size]
		if size >= 256 {
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				return nil, err
			}
			out = append(out, icoEntry{size, buf.Bytes()})
			continue
		}
		var buf bytes.Buffer
		le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
		// BITMAPINFOHEADER: высота удвоена — за цветом идёт маска.
		le(uint32(40))
		le(int32(size))
		le(int32(2 * size))
		le(uint16(1))
		le(uint16(32))
		le(uint32(0))
		le(uint32(size * size * 4))
		le([4]uint32{})
		for y := size - 1; y >= 0; y-- {
			for x := 0; x < size; x++ {
				c := img.NRGBAAt(x, y)
				buf.Write([]byte{c.B, c.G, c.R, c.A})
			}
		}
		// Маска AND: строки по 32 бита; прозрачность задаёт альфа-канал.
		buf.Write(make([]byte, size*((size+31)/32*4)))
		out = append(out, icoEntry{size, buf.Bytes()})
	}
	return out, nil
}

// dim — размер в заголовке значка: 256 пишется нулём.
func dim(size int) uint8 {
	if size >= 256 {
		return 0
	}
	return uint8(size)
}

// ico — файл .ico: заголовок, оглавление и картинки подряд.
func ico(entries []icoEntry) []byte {
	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	le([3]uint16{0, 1, uint16(len(entries))})
	offset := 6 + 16*len(entries)
	for _, e := range entries {
		buf.Write([]byte{dim(e.size), dim(e.size), 0, 0})
		le([2]uint16{1, 32})
		le(uint32(len(e.data)))
		le(uint32(offset))
		offset += len(e.data)
	}
	for _, e := range entries {
		buf.Write(e.data)
	}
	return buf.Bytes()
}

// Ресурсы Windows.
const (
	rtIcon      = 3
	rtGroupIcon = 14
	langNeutral = 0

	machineAMD64     = 0x8664
	relAMD64Addr32NB = 3 // адрес относительно начала образа
)

// groupIcon — ресурс RT_GROUP_ICON: то же оглавление, что в .ico, но вместо
// смещения — номер ресурса RT_ICON.
func groupIcon(entries []icoEntry) []byte {
	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	le([3]uint16{0, 1, uint16(len(entries))})
	for i, e := range entries {
		buf.Write([]byte{dim(e.size), dim(e.size), 0, 0})
		le([2]uint16{1, 32})
		le(uint32(len(e.data)))
		le(uint16(i + 1))
	}
	return buf.Bytes()
}

// syso — объектный файл COFF с одной секцией .rsrc: дерево ресурсов
// (вид → номер → язык), записи данных и сами данные. Адрес данных в образе
// компоновщик подставляет сам — по перемещению у каждой записи.
func syso(entries []icoEntry, machine uint16) []byte {
	type resource struct {
		kind, id uint32
		data     []byte
	}
	var resources []resource
	for i, e := range entries {
		resources = append(resources, resource{rtIcon, uint32(i + 1), e.data})
	}
	resources = append(resources, resource{rtGroupIcon, 1, groupIcon(entries)})

	// Размеры частей: корень со своими записями, каталог каждого вида,
	// каталог каждого ресурса (одна запись — язык), записи данных, данные.
	const dirSize, entrySize, dataEntrySize = 16, 8, 16
	kinds := []uint32{rtIcon, rtGroupIcon}
	count := map[uint32]int{}
	for _, r := range resources {
		count[r.kind]++
	}
	kindDir := map[uint32]int{}
	offset := dirSize + entrySize*len(kinds)
	for _, k := range kinds {
		kindDir[k] = offset
		offset += dirSize + entrySize*count[k]
	}
	langDir := offset
	dataEntries := langDir + (dirSize+entrySize)*len(resources)
	dataStart := dataEntries + dataEntrySize*len(resources)

	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	dir := func(ids int) {
		le([3]uint32{}) // признаки, время, версия
		le([2]uint16{0, uint16(ids)})
	}
	const subdirectory = 0x80000000
	dir(len(kinds))
	for _, k := range kinds {
		le([2]uint32{k, subdirectory | uint32(kindDir[k])})
	}
	for _, k := range kinds {
		dir(count[k])
		for i, r := range resources {
			if r.kind == k {
				le([2]uint32{r.id, subdirectory | uint32(langDir+(dirSize+entrySize)*i)})
			}
		}
	}
	for i := range resources {
		dir(1)
		le([2]uint32{langNeutral, uint32(dataEntries + dataEntrySize*i)})
	}
	var relocations []uint32
	at := dataStart
	for _, r := range resources {
		relocations = append(relocations, uint32(buf.Len()))
		le([4]uint32{uint32(at), uint32(len(r.data)), 0, 0})
		at += (len(r.data) + 7) &^ 7
	}
	for _, r := range resources {
		buf.Write(r.data)
		buf.Write(make([]byte, (8-len(r.data)%8)%8))
	}
	section := buf.Bytes()

	const fileHeader, sectionHeader, relocationSize = 20, 40, 10
	rawData := fileHeader + sectionHeader
	relocAt := rawData + len(section)
	symbols := relocAt + relocationSize*len(relocations)

	var out bytes.Buffer
	le = func(v any) { _ = binary.Write(&out, binary.LittleEndian, v) }
	name := [8]byte{'.', 'r', 's', 'r', 'c'}
	// Заголовок файла: одна секция, один символ.
	le(machine)
	le(uint16(1))
	le(uint32(0))
	le(uint32(symbols))
	le(uint32(1))
	le([2]uint16{0, 0})
	// Заголовок секции: инициализированные данные, чтение.
	le(name)
	le([2]uint32{0, 0})
	le(uint32(len(section)))
	le(uint32(rawData))
	le(uint32(relocAt))
	le(uint32(0))
	le([2]uint16{uint16(len(relocations)), 0})
	le(uint32(0x40000040))
	out.Write(section)
	for _, r := range relocations {
		le(r)
		le(uint32(0))
		le(uint16(relAMD64Addr32NB))
	}
	// Символ секции и пустая таблица строк.
	le(name)
	le(uint32(0))
	le(int16(1))
	le(uint16(0))
	out.Write([]byte{3, 0})
	le(uint32(4))
	return out.Bytes()
}
