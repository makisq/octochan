package lang

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Трассировка сценария (ochan run --trace): каждая инструкция — файл,
// строка и её текст, затем результат; вход в алиас и выход из него; при
// ошибке — значения переменных в этот момент и стек вызовов. Значение
// переменной, похожей на секрет (token, password, secret…), скрыто.

// tracer — общий для интерпретатора и его итераций parallel for.
type tracer struct {
	mu      sync.Mutex
	w       io.Writer
	sources map[string][]string
	// shown — ошибка, переменные при которой уже напечатаны: ошибка
	// поднимается через все вложенные инструкции, а печатать её надо раз.
	shown error
}

// SetTrace включает трассировку в w; sources — тексты сценариев по имени
// файла (как в позициях). Текст файла, которого нет в sources, читается с
// диска; не прочитался — печатается только позиция.
func (in *Interpreter) SetTrace(w io.Writer, sources map[string]string) {
	if w == nil {
		in.trace = nil
		return
	}
	t := &tracer{w: w, sources: map[string][]string{}}
	for name, text := range sources {
		t.sources[name] = strings.Split(text, "\n")
	}
	in.trace = t
}

// secretName — последнее слово имени значит секрет: token, api_key,
// smtp_password, session-cookie.
var secretName = regexp.MustCompile(`(?i)(^|[_.-])(token|tokens|password|passwd|pass|pwd|secret|secrets|cookie|cookies|credential|credentials|key|apikey|auth|authorization)$`)

// SecretName сообщает, что имя (переменной, настройки, поля) похоже на
// секрет: его значение не печатают ни трассировка, ни config show.
func SecretName(name string) bool {
	return secretName.MatchString(strings.TrimSpace(name))
}

// traceValue — значение для трассировки: одной строкой, не длиннее 120
// символов.
func traceValue(name string, v Value) string {
	if v == nil {
		return "null"
	}
	if name != "" && SecretName(name) {
		return "…"
	}
	text := strings.ReplaceAll(Display(v), "\n", `\n`)
	if utf8.RuneCountInString(text) > 120 {
		runes := []rune(text)
		text = string(runes[:117]) + "…"
	}
	if _, isString := v.(StringValue); isString {
		text = fmt.Sprintf("%q", text)
	}
	return text
}

func (t *tracer) line(pos Position) string {
	lines, ok := t.sources[pos.File]
	if !ok {
		if data, err := os.ReadFile(pos.File); err == nil {
			lines = strings.Split(string(data), "\n")
		}
		t.sources[pos.File] = lines
	}
	if pos.Line < 1 || pos.Line > len(lines) {
		return ""
	}
	return strings.TrimSpace(lines[pos.Line-1])
}

func (in *Interpreter) tracef(format string, args ...any) {
	t := in.trace
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.w, "%s"+format+"\n", append([]any{strings.Repeat("  ", len(in.stack))}, args...)...)
}

// traceStmt печатает инструкцию перед выполнением. Блок не печатается:
// печатаются его инструкции.
func (in *Interpreter) traceStmt(stmt Stmt) {
	if _, block := stmt.(*Block); block {
		return
	}
	pos := stmt.Position()
	in.trace.mu.Lock()
	text := in.trace.line(pos)
	in.trace.mu.Unlock()
	// Секрет, вписанный в сценарий, не уходит в трассировку и с текстом
	// строки: var api_token = "…".
	if assign, ok := stmt.(*Assign); ok && SecretName(assign.Name) {
		if i := strings.Index(text, "="); i >= 0 {
			text = strings.TrimSpace(text[:i]) + " = …"
		}
	}
	in.tracef("trace %s: %s", pos, text)
}

// traceResult печатает итог инструкции: значение выражения, новое
// значение переменной или ошибку с переменными в этот момент.
func (in *Interpreter) traceResult(stmt Stmt, env *scope, err error) {
	if err != nil {
		var ret returnSignal
		if errors.As(err, &ret) {
			in.tracef("  → return %s", traceValue("", ret.value))
			return
		}
		if errors.Is(err, errBreak) || errors.Is(err, errContinue) {
			return
		}
		in.trace.mu.Lock()
		first := in.trace.shown != err
		in.trace.shown = err
		in.trace.mu.Unlock()
		if !first {
			return
		}
		in.tracef("  ✗ %v", err)
		if vars := env.traceVars(); vars != "" {
			in.tracef("    переменные: %s", vars)
		}
		if len(in.stack) > 0 {
			in.tracef("    вызовы: %s", strings.Join(in.stack, " → "))
		}
		return
	}
	switch s := stmt.(type) {
	case *Assign:
		if value, ok := env.lookup(s.Name); ok {
			in.tracef("  → %s = %s", s.Name, traceValue(s.Name, value))
		}
	case *SetStmt:
		in.tracef("  → изменено")
	}
}

// traceExpr — значение выражения-инструкции, если оно не null: print()
// и вызовы без результата результата не печатают.
func (in *Interpreter) traceExpr(v Value) {
	if v == nil {
		return
	}
	if _, isNull := v.(NullValue); isNull {
		return
	}
	in.tracef("  → %s", traceValue("", v))
}

// traceCall — вход в алиас с аргументами.
func (in *Interpreter) traceCall(decl *AliasDecl, args []Value) {
	parts := make([]string, len(args))
	for i, arg := range args {
		name := ""
		if i < len(decl.Params) {
			name = decl.Params[i]
		}
		parts[i] = traceValue(name, arg)
	}
	in.tracef("→ вызов %s(%s)", decl.Name, strings.Join(parts, ", "))
}

// traceVars — переменные области и внешних областей, ближние сильнее, по
// имени.
func (s *scope) traceVars() string {
	seen := map[string]bool{}
	var names []string
	values := map[string]Value{}
	for cur := s; cur != nil; cur = cur.parent {
		for name, value := range cur.vars {
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
			values[name] = value
		}
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = name + " = " + traceValue(name, values[name])
	}
	return strings.Join(parts, ", ")
}
