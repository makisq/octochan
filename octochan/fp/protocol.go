package fp

import (
	_ "embed"
	"strings"
)

//go:embed protocol.md
var embedded string

// protocol — встроенный текст с переводами строк LF. Клон на Windows с
// core.autocrlf выгружает protocol.md с CRLF, и без приведения такой \r
// попадал в бинарь: строка «```\r» уже не закрывала блок примера.
var protocol = strings.ReplaceAll(embedded, "\r\n", "\n")

// Protocol возвращает текст протокола — то, что модель читает первым:
// кто она здесь, на каком языке с ней говорят и как она отвечает.
func Protocol() string { return protocol }
