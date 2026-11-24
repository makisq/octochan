package cmd

import (
	"bytes"
	"testing"

	"github.com/c-bata/go-prompt"
)

// Границы слов — в символах: кириллица, пути, знаки препинания, пробелы.
func TestWordBoundaries(t *testing.T) {
	before := []struct {
		text string
		n    int
	}{
		{"ochan apply", 5},
		{"ochan apply   ", 8},
		{"привет мир", 3},
		{"D:/ochan-demo/pg", 2},
		{"D:/ochan-demo/", 1},
		{"--request", 7},
		{"apply --", 2},
		{"", 0},
		{"   ", 3},
		{"work_mem_64mb", 13},
	}
	for _, c := range before {
		if got := wordBefore([]rune(c.text)); got != c.n {
			t.Errorf("wordBefore(%q) = %d, ожидалось %d", c.text, got, c.n)
		}
	}
	after := []struct {
		text string
		n    int
	}{
		{"apply --request", 5},
		{"  мир привет", 5},
		{"/pg/postgresql.conf", 1},
		{"", 0},
	}
	for _, c := range after {
		if got := wordAfter([]rune(c.text)); got != c.n {
			t.Errorf("wordAfter(%q) = %d, ожидалось %d", c.text, got, c.n)
		}
	}
}

// Нажатия сводятся к клавишам слов; Backspace и вставка текста — нет.
func TestTranslateWordKeys(t *testing.T) {
	cases := []struct {
		name          string
		in            []byte
		ctrlBackspace byte
		want          []byte
	}{
		{"Windows: Ctrl+Backspace (0x7F)", []byte{0x7f}, 0x7f, seqDeleteWordBack},
		{"Windows: Backspace (0x08) остаётся", []byte{0x08}, 0x7f, []byte{0x08}},
		{"Linux: Ctrl+Backspace (0x08)", []byte{0x08}, 0x08, seqDeleteWordBack},
		{"Linux: Backspace (0x7F) остаётся", []byte{0x7f}, 0x08, []byte{0x7f}},
		{"stty erase ^H: 0x08 — Backspace", []byte{0x08}, 0, []byte{0x08}},
		{"macOS: Option+Backspace", []byte{0x1b, 0x7f}, 0x08, seqDeleteWordBack},
		{"macOS: Option+←", []byte("\x1bb"), 0x08, seqWordLeft},
		{"macOS: Option+→", []byte("\x1bf"), 0x08, seqWordRight},
		{"readline: Esc d", []byte("\x1bd"), 0x08, seqDeleteWordAfter},
		{"xterm: Alt+←", []byte("\x1b[1;3D"), 0x08, seqWordLeft},
		{"iTerm2: Option+→", []byte("\x1b[1;9C"), 0x08, seqWordRight},
		{"Ctrl+← как есть", seqWordLeft, 0x7f, seqWordLeft},
		{"вставка текста не трогается", []byte("b\x7f"), 0x7f, []byte("b\x7f")},
		{"буква", []byte("b"), 0x08, []byte("b")},
	}
	for _, c := range cases {
		if got := translateWordKeys(c.in, c.ctrlBackspace); !bytes.Equal(got, c.want) {
			t.Errorf("%s: %q → %q, ожидалось %q", c.name, c.in, got, c.want)
		}
	}
	// Ctrl+←, Ctrl+→, Ctrl+Delete go-prompt узнаёт сам; ESC DEL — нет, её
	// ловит привязка к байтам.
	for seq, key := range map[string]prompt.Key{string(seqWordLeft): prompt.ControlLeft,
		string(seqWordRight): prompt.ControlRight, string(seqDeleteWordAfter): prompt.ControlDelete} {
		if got := prompt.GetKey([]byte(seq)); got != key {
			t.Errorf("GetKey(%q) = %v, ожидалось %v", seq, got, key)
		}
	}
	if got := prompt.GetKey(seqDeleteWordBack); got != prompt.NotDefined {
		t.Errorf("ESC DEL go-prompt знает как %v: привязка к байтам не сработает", got)
	}
}

// Правка строки: курсор и удаление по словам, кириллица цела.
func TestWordEditing(t *testing.T) {
	buf := prompt.NewBuffer()
	buf.InsertText("ochan approve заявка-42 сейчас", false, true)

	deleteWordBack(buf)
	if got := buf.Text(); got != "ochan approve заявка-42 " {
		t.Fatalf("Ctrl+Backspace: %q", got)
	}
	deleteWordBack(buf)
	if got := buf.Text(); got != "ochan approve заявка-" {
		t.Fatalf("Ctrl+Backspace по числу: %q", got)
	}

	wordLeft(buf) // перед «-»
	wordLeft(buf) // перед «заявка»
	if got := buf.Document().TextBeforeCursor(); got != "ochan approve " {
		t.Fatalf("Ctrl+← дважды: курсор после %q", got)
	}
	deleteWordAfter(buf)
	if got := buf.Text(); got != "ochan approve -" {
		t.Fatalf("Ctrl+Delete: %q", got)
	}
	wordLeft(buf)
	wordRight(buf)
	if got := buf.Document().TextBeforeCursor(); got != "ochan approve" {
		t.Fatalf("Ctrl+← и Ctrl+→: курсор после %q", got)
	}
}
