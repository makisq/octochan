package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"image"
	"image/png"
	"os"
	"testing"
)

func logo(t *testing.T) *image.NRGBA {
	t.Helper()
	f, err := os.Open("../../docs/assets/logo.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	icon, err := emblem(img)
	if err != nil {
		t.Fatal(err)
	}
	return icon
}

// Эмблема — квадрат без надписи: углы прозрачны, низ — фон логотипа, а не
// верх букв.
func TestEmblem(t *testing.T) {
	icon := logo(t)
	side := icon.Bounds().Dx()
	if side != icon.Bounds().Dy() || side < 256 {
		t.Fatalf("эмблема %dx%d", side, icon.Bounds().Dy())
	}
	if icon.NRGBAAt(0, 0).A != 0 || icon.NRGBAAt(side/2, side/2).A != 255 {
		t.Fatal("углы не скруглены")
	}
	background := icon.NRGBAAt(side/2, 2)
	for x := side / 4; x < 3*side/4; x++ {
		if c := icon.NRGBAAt(x, side-3); c.R != background.R || c.G != background.G || c.B != background.B {
			t.Fatalf("внизу значка — не фон: %v в столбце %d (край надписи?)", c, x)
		}
	}
	small := resize(icon, 16)
	if small.Bounds().Dx() != 16 || small.NRGBAAt(8, 8).A != 255 || small.NRGBAAt(0, 0).A > 40 {
		t.Fatal("значок 16×16 негоден")
	}
}

// Ресурс Windows — объектный файл, который разбирает компоновщик: секция
// .rsrc, её символ и перемещение у каждой записи данных; в дереве ресурсов —
// значки всех размеров и их оглавление.
func TestSyso(t *testing.T) {
	icon := logo(t)
	images := map[int]*image.NRGBA{}
	for _, size := range iconSizes {
		images[size] = resize(icon, size)
	}
	entries, err := icoEntries(images)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"собранный": syso(entries, machineAMD64), "в репозитории": committed(t)} {
		f, err := pe.NewFile(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if f.Machine != machineAMD64 || len(f.Sections) != 1 || f.Sections[0].Name != ".rsrc" || len(f.Symbols) != 1 ||
			f.Symbols[0].Name != ".rsrc" {
			t.Fatalf("%s: машина %#x, секций %d, символов %d", name, f.Machine, len(f.Sections), len(f.Symbols))
		}
		section := f.Sections[0]
		raw, err := section.Data()
		if err != nil {
			t.Fatal(err)
		}
		// Корень: два вида ресурсов — RT_ICON и RT_GROUP_ICON.
		kinds := binary.LittleEndian.Uint16(raw[14:])
		first, second := binary.LittleEndian.Uint32(raw[16:]), binary.LittleEndian.Uint32(raw[24:])
		if kinds != 2 || first != rtIcon || second != rtGroupIcon {
			t.Fatalf("%s: виды ресурсов %d: %d, %d", name, kinds, first, second)
		}
		iconDir := binary.LittleEndian.Uint32(raw[20:]) &^ 0x80000000
		if n := binary.LittleEndian.Uint16(raw[iconDir+14:]); int(n) != len(iconSizes) {
			t.Fatalf("%s: значков %d, размеров %d", name, n, len(iconSizes))
		}
		if len(section.Relocs) != len(iconSizes)+1 {
			t.Fatalf("%s: перемещений %d на %d записей данных", name, len(section.Relocs), len(iconSizes)+1)
		}
		for _, r := range section.Relocs {
			if r.Type != relAMD64Addr32NB || r.SymbolTableIndex != 0 {
				t.Fatalf("%s: перемещение %+v", name, r)
			}
			// Запись данных: смещение внутри секции и размер — в её пределах.
			at, size := binary.LittleEndian.Uint32(raw[r.VirtualAddress:]), binary.LittleEndian.Uint32(raw[r.VirtualAddress+4:])
			if int(at+size) > len(raw) || size == 0 {
				t.Fatalf("%s: данные %d+%d вне секции %d", name, at, size, len(raw))
			}
		}
	}
	if file := ico(entries); binary.LittleEndian.Uint16(file[4:]) != uint16(len(iconSizes)) {
		t.Fatal("оглавление .ico")
	}
}

func committed(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../rsrc_windows_amd64.syso")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Значок, который несёт ochan, — 256×256 с прозрачными углами.
func TestCommittedPNG(t *testing.T) {
	f, err := os.Open("../../cmd/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 256 || b.Dy() != 256 {
		t.Fatalf("cmd/icon.png: %dx%d", b.Dx(), b.Dy())
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Fatal("cmd/icon.png: углы непрозрачны")
	}
}
