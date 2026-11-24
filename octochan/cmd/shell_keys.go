package cmd

import (
	"bytes"
	"unicode"

	"github.com/c-bata/go-prompt"
)

// Слова в строке оболочки — как в редакторах и браузерах:
//
//	Ctrl+←, Ctrl+→            — на слово назад и вперёд;
//	Ctrl+Backspace            — стереть слово перед курсором;
//	Ctrl+Delete               — стереть слово после курсора.
//
// На macOS то же — Option+←, Option+→, Option+Backspace (и Esc b, Esc f,
// Esc d, как в readline).
//
// Слово — буквы, цифры и _; серия знаков препинания — тоже слово, пробелы
// пропускаются: в «D:/ochan-demo/pg» Ctrl+Backspace снимает «pg», потом
// «/», потом «demo». Счёт — в символах, а не в байтах: функции слов
// go-prompt путают одно с другим, и на кириллице курсор прыгал не туда.
//
// Терминалы шлют эти клавиши по-разному, и Ctrl+Backspace хуже всех:
// консоль Windows отдаёт его как 0x7F, а обычный Backspace — как 0x08;
// терминалы Linux и macOS — наоборот. Поэтому сырые байты сначала
// сводятся к одному набору последовательностей (wordKeysParser), а уже их
// разбирают привязки.

// Последовательности, к которым сводятся клавиши слов. Первые три go-prompt
// знает сам (ControlLeft, ControlRight, ControlDelete) и ничего к ним не
// привязывает; ESC DEL он не знает — она ловится привязкой к байтам.
var (
	seqWordLeft        = []byte("\x1b[1;5D")
	seqWordRight       = []byte("\x1b[1;5C")
	seqDeleteWordAfter = []byte("\x1b[3;5~")
	seqDeleteWordBack  = []byte{0x1b, 0x7f}
)

// wordKeyAliases — другие записи тех же клавиш: Alt/Option+стрелки xterm и
// iTerm2, Esc b / Esc f / Esc d readline, Alt+Delete, Alt+Ctrl+Backspace.
var wordKeyAliases = []struct{ from, to []byte }{
	{[]byte("\x1bb"), seqWordLeft},
	{[]byte("\x1bf"), seqWordRight},
	{[]byte("\x1bd"), seqDeleteWordAfter},
	{[]byte("\x1b[1;3D"), seqWordLeft},
	{[]byte("\x1b[1;3C"), seqWordRight},
	{[]byte("\x1b[1;9D"), seqWordLeft},
	{[]byte("\x1b[1;9C"), seqWordRight},
	{[]byte("\x1b[3;3~"), seqDeleteWordAfter},
	{[]byte{0x1b, 0x08}, seqDeleteWordBack},
}

// translateWordKeys сводит одно нажатие к последовательности клавиш слов.
// ctrlBackspace — байт, которым этот терминал шлёт Ctrl+Backspace (0 — не
// различить с Backspace). Пачка байтов — вставка текста или быстрый ввод —
// не трогается: go-prompt разбирает только нажатие целиком.
func translateWordKeys(b []byte, ctrlBackspace byte) []byte {
	if ctrlBackspace != 0 && len(b) == 1 && b[0] == ctrlBackspace {
		return seqDeleteWordBack
	}
	for _, a := range wordKeyAliases {
		if bytes.Equal(b, a.from) {
			return a.to
		}
	}
	return b
}

// wordKeysParser — ввод go-prompt со сведёнными клавишами слов.
type wordKeysParser struct {
	prompt.ConsoleParser
	ctrlBackspace byte
}

func (p *wordKeysParser) Read() ([]byte, error) {
	b, err := p.ConsoleParser.Read()
	if err != nil {
		return b, err
	}
	return translateWordKeys(b, p.ctrlBackspace), nil
}

// wordKeyOptions — привязки клавиш слов для go-prompt.
func wordKeyOptions() []prompt.Option {
	return []prompt.Option{
		prompt.OptionAddKeyBind(
			prompt.KeyBind{Key: prompt.ControlLeft, Fn: wordLeft},
			prompt.KeyBind{Key: prompt.ControlRight, Fn: wordRight},
			prompt.KeyBind{Key: prompt.ControlDelete, Fn: deleteWordAfter},
		),
		prompt.OptionAddASCIICodeBind(prompt.ASCIICodeBind{ASCIICode: seqDeleteWordBack, Fn: deleteWordBack}),
	}
}

func wordLeft(buf *prompt.Buffer) {
	buf.CursorLeft(wordBefore([]rune(buf.Document().TextBeforeCursor())))
}

func wordRight(buf *prompt.Buffer) {
	buf.CursorRight(wordAfter([]rune(buf.Document().TextAfterCursor())))
}

func deleteWordBack(buf *prompt.Buffer) {
	buf.DeleteBeforeCursor(wordBefore([]rune(buf.Document().TextBeforeCursor())))
}

// deleteWordAfter: Buffer.Delete go-prompt режет строку по байтам, поэтому
// — курсор за слово и стереть перед ним.
func deleteWordAfter(buf *prompt.Buffer) {
	n := wordAfter([]rune(buf.Document().TextAfterCursor()))
	buf.CursorRight(n)
	buf.DeleteBeforeCursor(n)
}

// Классы символов для границ слова.
const (
	classSpace = iota
	classWord
	classPunct
)

func runeClass(r rune) int {
	switch {
	case unicode.IsSpace(r):
		return classSpace
	case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
		return classWord
	}
	return classPunct
}

// wordBefore — сколько символов до начала слова перед курсором: пробелы,
// затем серия символов одного класса.
func wordBefore(before []rune) int {
	i := len(before)
	for i > 0 && runeClass(before[i-1]) == classSpace {
		i--
	}
	if i > 0 {
		class := runeClass(before[i-1])
		for i > 0 && runeClass(before[i-1]) == class {
			i--
		}
	}
	return len(before) - i
}

// wordAfter — сколько символов до конца слова после курсора.
func wordAfter(after []rune) int {
	i := 0
	for i < len(after) && runeClass(after[i]) == classSpace {
		i++
	}
	if i < len(after) {
		class := runeClass(after[i])
		for i < len(after) && runeClass(after[i]) == class {
			i++
		}
	}
	return i
}
