// Package own — своя модель ℒ: словарь, сеть и архитектурная генерация,
// где логика и кольца — те же пакеты fp и rings, что у ochan. Модель
// исполняется в процессе ochan; второй реализации логики или колец нет.
package own

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"ochan/fp"
)

// Словарь своей модели: алфавит ℒ, частные имена — переменные.
//
// Статистика модели — только по конечному словарю языка: слова словаря и
// их смыслы-классы, служебные и ключевые слова, единицы, знаки грамматики
// (ochan fp alphabet), цифры и разметка строки. Составное имя со смыслом
// (connection_limit) — слова через «_».
//
// Всё прочее — имена частных случаев: значения слотов, имена стрелок,
// ячеек, версий, путей binds_to. Их смысл задаёт не словарь, а место в
// структуре. Поэтому имя частного случая — переменная <vK>, K — номер его
// первого появления в тексте (задача, затем ответ). Модель не учит, что
// такое plod_ser2222_ru, а учит, куда переносится переменная; ответ по
// построению не меняется, когда меняется только имя (критерий А.6
// манифеста, пункт а).
//
// Текст сначала приводится к канонической записи (fp.Canonical). У каждого
// токена есть форма с пробелом впереди (« GB», « set», « <v3>»), у слов и
// цифр — с «_» впереди («_nightly»): отдельный пробел или «_» между
// токенами модели пришлось бы перешагивать при копировании. Словарь без
// этих форм читается по-прежнему: формы берутся, только если они в словаре.
//
// Новое имя, которого не было в задаче (ячейка с решениями), модель пишет
// следующей свободной переменной; на письме это cK со свободным K — так
// ячейки называет и fp.PlanAnswer.

// TokenizerFile — словарь в каталоге модели.
const TokenizerFile = "fpmtok.json"

// Служебные токены.
const (
	Pad    = "<pad>"
	Bos    = "<bos>"
	Answer = "<answer>"
	Eos    = "<eos>"
)

// MaxVariables — переменных в словаре; имя дальше — байтами.
const MaxVariables = 64

var (
	special  = []string{Pad, Bos, Answer, Eos}
	layout   = []string{"\n", "  ", " "}
	digits   = strings.Split("0123456789", "")
	extra    = []string{`"`, "_", "n"} // «_» склеивает составное имя; n — приставка ??n_
	variable = regexp.MustCompile(`^( ?)<v(\d+)>$`)
	byteTok  = regexp.MustCompile(`^<0x([0-9A-F]{2})>$`)
)

// Names — имена частных случаев одного текста: номер переменной ↔ имя.
type Names struct {
	list  []string
	index map[string]int
}

// NewNames — имена по порядку номеров.
func NewNames(names ...string) *Names {
	n := &Names{index: map[string]int{}}
	for _, name := range names {
		n.Of(name)
	}
	return n
}

// Copy — независимая копия.
func (n *Names) Copy() *Names { return NewNames(n.list...) }

// Len — сколько имён.
func (n *Names) Len() int { return len(n.list) }

// List — имена по номерам.
func (n *Names) List() []string { return append([]string(nil), n.list...) }

// Of — номер переменной имени; новое имя — следующий номер.
func (n *Names) Of(name string) int {
	if k, ok := n.index[name]; ok {
		return k
	}
	n.index[name] = len(n.list)
	n.list = append(n.list, name)
	return len(n.list) - 1
}

// Name — имя переменной k; следующая свободная — новое имя cK.
func (n *Names) Name(k int) string {
	if k == len(n.list) {
		for c := 1; ; c++ {
			if fresh := "c" + strconv.Itoa(c); !n.has(fresh) {
				n.Of(fresh)
				break
			}
		}
	}
	if k < 0 || k >= len(n.list) {
		return ""
	}
	return n.list[k]
}

func (n *Names) has(name string) bool { _, ok := n.index[name]; return ok }

// varTok — переменная: пробел впереди и номер.
type varTok struct {
	space string
	k     int
}

// Tokenizer — словарь модели.
type Tokenizer struct {
	Tokens                []string
	ids                   map[string]int
	Pad, Bos, Answer, Eos int
	variable              map[int]varTok
	bytes                 map[int]byte
	closed                map[string]bool
	puncts                []string
}

// NewTokenizer — словарь из списка токенов.
func NewTokenizer(tokens []string) (*Tokenizer, error) {
	t := &Tokenizer{Tokens: tokens, ids: map[string]int{}, variable: map[int]varTok{}, bytes: map[int]byte{}, closed: map[string]bool{}}
	for i, tok := range tokens {
		t.ids[tok] = i
	}
	for _, s := range special {
		if _, ok := t.ids[s]; !ok {
			return nil, fmt.Errorf("в словаре нет служебного токена %s", s)
		}
	}
	t.Pad, t.Bos, t.Answer, t.Eos = t.ids[Pad], t.ids[Bos], t.ids[Answer], t.ids[Eos]
	isSpecial := map[string]bool{}
	for _, s := range special {
		isSpecial[s] = true
	}
	for i, tok := range tokens {
		if m := variable.FindStringSubmatch(tok); m != nil {
			k, _ := strconv.Atoi(m[2])
			t.variable[i] = varTok{m[1], k}
			continue
		}
		if m := byteTok.FindStringSubmatch(tok); m != nil {
			b, _ := strconv.ParseUint(m[1], 16, 8)
			t.bytes[i] = byte(b)
			continue
		}
		if !isSpecial[tok] {
			t.closed[tok] = true
		}
	}
	isLayout := map[string]bool{}
	for _, l := range layout {
		isLayout[l] = true
	}
	for tok := range t.closed {
		if !isIdent(tok) && !isLayout[tok] && !isDigitString(tok) && !strings.HasPrefix(tok, " ") && !strings.HasPrefix(tok, "_") {
			t.puncts = append(t.puncts, tok)
		}
	}
	sort.Slice(t.puncts, func(i, j int) bool {
		if len(t.puncts[i]) != len(t.puncts[j]) {
			return len(t.puncts[i]) > len(t.puncts[j])
		}
		return t.puncts[i] < t.puncts[j]
	})
	return t, nil
}

// AlphabetEntry — запись алфавита ℒ: вид и текст (ochan fp alphabet).
type AlphabetEntry struct{ Kind, Text string }

// BuildTokenizer — словарь новой модели из алфавита ℒ; forms — формы
// токенов с пробелом и «_» впереди.
func BuildTokenizer(alphabet []AlphabetEntry, forms bool) (*Tokenizer, error) {
	var words []string
	seen := map[string]bool{}
	for _, e := range alphabet {
		if e.Text != "" && !seen[e.Text] {
			seen[e.Text] = true
			words = append(words, e.Text)
		}
	}
	base := append([]string(nil), digits...)
	for _, t := range extra {
		if !seen[t] {
			base = append(base, t)
		}
	}
	base = append(base, words...)
	var variables []string
	for k := 0; k < MaxVariables; k++ {
		variables = append(variables, fmt.Sprintf("<v%d>", k))
	}
	tokens := append(append(append([]string(nil), special...), layout...), base...)
	if forms {
		for _, t := range base {
			if t != "_" {
				tokens = append(tokens, " "+t)
			}
		}
		for _, t := range base {
			if t != "_" && (isIdent(t) || isDigitString(t)) {
				tokens = append(tokens, "_"+t)
			}
		}
		for k := 0; k < MaxVariables; k++ {
			variables = append(variables, " "+variables[k])
		}
	}
	tokens = append(tokens, variables...)
	for b := 0; b < 256; b++ {
		tokens = append(tokens, fmt.Sprintf("<0x%02X>", b))
	}
	return NewTokenizer(tokens)
}

// ParseAlphabet — алфавит из выгрузки ochan fp alphabet: «вид<TAB>текст».
func ParseAlphabet(text string) []AlphabetEntry {
	var out []AlphabetEntry
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if kind, item, ok := strings.Cut(line, "\t"); ok {
			out = append(out, AlphabetEntry{kind, item})
		}
	}
	return out
}

type tokenizerFile struct {
	Tokens []string `json:"tokens"`
}

// LoadTokenizer — словарь из каталога модели.
func LoadTokenizer(dir string) (*Tokenizer, error) {
	data, err := os.ReadFile(filepath.Join(dir, TokenizerFile))
	if err != nil {
		return nil, err
	}
	var f tokenizerFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", TokenizerFile, err)
	}
	return NewTokenizer(f.Tokens)
}

// Save пишет словарь в каталог модели.
func (t *Tokenizer) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tokenizerFile{t.Tokens}, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, TokenizerFile), append(data, '\n'), 0o644)
}

// Len — размер словаря.
func (t *Tokenizer) Len() int { return len(t.Tokens) }

// Closed — текст токена — слово или знак словаря (не переменная, не байт,
// не служебный): так модель отличает слово языка от частного имени.
func (t *Tokenizer) Closed(text string) bool { return t.closed[text] }

// Variable — номер переменной токена; ok ложно — не переменная.
func (t *Tokenizer) Variable(token int) (int, bool) {
	v, ok := t.variable[token]
	return v.k, ok
}

// IsByte — токен-байт.
func (t *Tokenizer) IsByte(token int) bool { _, ok := t.bytes[token]; return ok }

func (t *Tokenizer) nameTexts(names *Names, name string) []string {
	if k := names.Of(name); k < MaxVariables {
		return []string{fmt.Sprintf("<v%d>", k)}
	}
	var out []string
	for _, b := range []byte(name) {
		out = append(out, fmt.Sprintf("<0x%02X>", b))
	}
	return out
}

// ident — слово словаря, составное имя из слов — или переменная.
func (t *Tokenizer) ident(word string, names *Names) []string {
	if t.closed[word] {
		return []string{word}
	}
	parts := strings.Split(word, "_")
	for _, p := range parts {
		if p != "" && !t.closed[p] && !isDigitString(p) {
			return t.nameTexts(names, word)
		}
	}
	var out []string
	for k, p := range parts {
		if k > 0 {
			out = append(out, "_")
		}
		switch {
		case isDigitString(p):
			for _, r := range p {
				out = append(out, string(r))
			}
		case p != "":
			out = append(out, p)
		}
	}
	return out
}

// texts — лексемы канонического текста: тексты токенов без форм,
// переменная — <vK>.
func (t *Tokenizer) texts(text string, names *Names) []string {
	var out []string
	for n, line := range strings.Split(text, "\n") {
		if n > 0 {
			out = append(out, "\n")
		}
		body := strings.TrimLeft(line, " ")
		indent := len(line) - len(body)
		for k := 0; k < indent/2; k++ {
			out = append(out, "  ")
		}
		if indent%2 == 1 {
			out = append(out, " ")
		}
		if rest, ok := strings.CutPrefix(body, "binds_to:"); ok {
			// Путь к коду — одно имя частного случая.
			path := strings.TrimSpace(rest)
			out = append(out, "binds_to", ":")
			if path != "" {
				out = append(out, " ")
				out = append(out, t.nameTexts(names, path)...)
			}
			continue
		}
		for i := 0; i < len(body); {
			r, size := utf8.DecodeRuneInString(body[i:])
			switch {
			case r == ' ' || unicode.IsDigit(r):
				out = append(out, body[i:i+size])
				i += size
			case isIdentStart(r):
				j := i + size
				for j < len(body) {
					r2, s2 := utf8.DecodeRuneInString(body[j:])
					if !isIdentRune(r2) {
						break
					}
					j += s2
				}
				out = append(out, t.ident(body[i:j], names)...)
				i = j
			default:
				found := ""
				for _, p := range t.puncts {
					if strings.HasPrefix(body[i:], p) {
						found = p
						break
					}
				}
				if found != "" {
					out = append(out, found)
					i += len(found)
					continue
				}
				for _, b := range []byte(body[i : i+size]) {
					out = append(out, fmt.Sprintf("<0x%02X>", b))
				}
				i += size
			}
		}
	}
	return out
}

// Encode — токены текста, уже приведённого к канонической записи: пробел
// и «_» входят в следующий токен, если такая форма есть в словаре.
func (t *Tokenizer) Encode(text string, names *Names) ([]int, error) {
	texts := t.texts(text, names)
	var out []int
	for i := 0; i < len(texts); i++ {
		tok := texts[i]
		if (tok == " " || tok == "_") && i+1 < len(texts) {
			if id, ok := t.ids[tok+texts[i+1]]; ok {
				out = append(out, id)
				i++
				continue
			}
		}
		id, ok := t.ids[tok]
		if !ok {
			return nil, fmt.Errorf("токена %q нет в словаре модели", tok)
		}
		out = append(out, id)
	}
	return out, nil
}

// Piece — текст одного токена; переменная — имя (новое — cK).
func (t *Tokenizer) Piece(token int, names *Names) string {
	if v, ok := t.variable[token]; ok {
		return v.space + names.Name(v.k)
	}
	if b, ok := t.bytes[token]; ok {
		return decodeBytes([]byte{b})
	}
	if token < 0 || token >= len(t.Tokens) {
		return ""
	}
	switch text := t.Tokens[token]; text {
	case Pad, Bos, Answer, Eos:
		return ""
	default:
		return text
	}
}

// Decode — текст токенов; подряд идущие байты — один кусок UTF-8.
func (t *Tokenizer) Decode(ids []int, names *Names) string {
	var b strings.Builder
	var raw []byte
	for _, id := range ids {
		if v, ok := t.bytes[id]; ok {
			raw = append(raw, v)
			continue
		}
		if raw != nil {
			b.WriteString(decodeBytes(raw))
			raw = nil
		}
		b.WriteString(t.Piece(id, names))
	}
	if raw != nil {
		b.WriteString(decodeBytes(raw))
	}
	return b.String()
}

// Pair — последовательность модели: <bos> задача <answer> [ответ <eos>].
// Возвращает токены, длину промпта и имена.
func (t *Tokenizer) Pair(task string, answer *string) ([]int, int, *Names, error) {
	names := NewNames()
	body, err := t.Encode(fp.Canonical(task), names)
	if err != nil {
		return nil, 0, nil, err
	}
	ids := append(append([]int{t.Bos}, body...), t.Answer)
	start := len(ids)
	if answer != nil {
		more, err := t.Encode(fp.Canonical(*answer), names)
		if err != nil {
			return nil, 0, nil, err
		}
		ids = append(append(ids, more...), t.Eos)
	}
	return ids, start, names, nil
}

// decodeBytes — UTF-8 с заменой негодного на U+FFFD, как Python errors="replace".
func decodeBytes(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		out.WriteRune(r)
		b = b[size:]
	}
	return out.String()
}

func isIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func isIdentRune(r rune) bool  { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && !isIdentStart(r) || i > 0 && !isIdentRune(r) {
			return false
		}
	}
	return true
}

func isDigitString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
