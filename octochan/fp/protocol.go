package fp

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed protocol.md
var embedded string

//go:embed protocol_natural.md
var embeddedNatural string

// Mode — как оператор ставит задачу модели.
type Mode string

const (
	// Formal — только формальным промптом: текст вне языка — ⊥syn.
	Formal Mode = "formal"
	// Natural — на естественном языке: формальный промпт не обязателен,
	// а жёстких правил, по которым проверить план, нет.
	Natural Mode = "natural"
)

// ParseMode разбирает fp.mode; пусто — Formal.
func ParseMode(text string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(text))); m {
	case "":
		return Formal, nil
	case Formal, Natural:
		return m, nil
	}
	return "", fmt.Errorf("fp.mode: %q, допустимо formal или natural", text)
}

// Тексты со встроенными переводами строк LF. Клон на Windows с
// core.autocrlf выгружает .md с CRLF, и без приведения такой \r попадал
// в бинарь: строка «```\r» уже не закрывала блок примера.
var (
	protocol        = strings.ReplaceAll(embedded, "\r\n", "\n")
	naturalProtocol = strings.ReplaceAll(embeddedNatural, "\r\n", "\n")
)

// Protocol возвращает текст протокола — то, что модель читает первым:
// кто она здесь, на каком языке с ней говорят и как она отвечает.
func Protocol() string { return protocol }

// ProtocolFor — протокол для режима: в режиме Natural модель принимает
// задачи на естественном языке.
func ProtocolFor(mode Mode) string {
	if mode == Natural {
		return naturalProtocol
	}
	return protocol
}
