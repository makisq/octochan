package fp

import _ "embed"

//go:embed protocol.md
var protocol string

// Protocol возвращает текст протокола — то, что модель читает первым:
// кто она здесь, на каком языке с ней говорят и как она отвечает.
func Protocol() string { return protocol }
