package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"

	"ochan/lang"
)

// Язык в оболочке тот же, что в файле, как у bash: строка — команда ochan
// или инструкция языка алиасов. Переменные и объявленные алиасы живут
// между строками, блок { … } пишут в несколько строк, а команды ochan из
// инструкций — ochan(). Право — как у eval (группа apply): инструкция в
// консоли выполняет то же, что сценарий; команды — их собственные права.
//
//	ochan> logs rank --min warning           # команда
//	ochan> n = len(split(ochan("agents"), "\n"))
//	ochan> if n > 3 {
//	...      print("агентов: " + str(n))
//	...    }

var (
	shellSession = lang.NewSession()
	// shellDecls — алиасы, записи и интерфейсы, объявленные в оболочке.
	shellDecls = &lang.File{Name: "<shell>"}
	// shellPending — начало инструкции, блок которой ещё не закрыт.
	shellPending string
)

// shellReset — новый сеанс языка: без переменных и объявлений оболочки
// (!!reset).
func shellReset() {
	shellSession, shellDecls, shellPending = lang.NewSession(), &lang.File{Name: "<shell>"}, ""
}

// shellStatement решает, инструкция ли строка. Слово, которое и команда,
// и имя в языке, — команда, если за ним не идёт ( или =: diff a.conf
// b.conf — команда, diff("a.conf", "b.conf") — вызов. Ключевое слово языка
// (if, for, alias) — инструкция, если строка разбирается языком: if x > 1 {
// … } — инструкция, if config.conf port == 8080 "ок" — команда if.
func shellStatement(line string) bool {
	if strings.HasPrefix(line, "!") {
		return false
	}
	word, rest := leadingWord(line)
	next := strings.TrimLeft(rest, " \t")
	if isCommandWord(word) && !lang.IsKeyword(word) {
		return strings.HasPrefix(next, "(") || strings.HasPrefix(next, "=") && !strings.HasPrefix(next, "==")
	}
	if blockDepth(line) > 0 {
		return true
	}
	file, err := lang.Parse("<shell>", line)
	if err != nil {
		// Не команда — значит, ошибка в инструкции: её и покажет язык.
		return !isCommandWord(word)
	}
	// Голое слово — переменная сеанса или команда (неизвестную назовёт
	// ochan).
	if len(file.Stmts) == 1 && len(file.Aliases)+len(file.Records)+len(file.Interfaces) == 0 {
		if stmt, ok := file.Stmts[0].(*lang.ExprStmt); ok {
			if id, ok := stmt.Expr.(*lang.Ident); ok && id.Module == "" {
				return shellSession.Defined(id.Name)
			}
		}
	}
	return true
}

func leadingWord(line string) (string, string) {
	end := strings.IndexFunc(line, func(r rune) bool { return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') })
	if end < 0 {
		return line, ""
	}
	return line[:end], line[end:]
}

// isCommandWord — у ochan есть команда с таким именем.
func isCommandWord(word string) bool {
	if word == "" {
		return false
	}
	target, _, err := rootCmd.Find([]string{word})
	return err == nil && target != rootCmd
}

// blockDepth — сколько скобок осталось открыто: вне строк и комментариев.
func blockDepth(src string) int {
	depth, quote := 0, rune(0)
	for _, line := range strings.Split(src, "\n") {
		for _, r := range line {
			switch {
			case quote != 0:
				if r == quote {
					quote = 0
				}
			case r == '"' || r == '\'':
				quote = r
			case r == '#':
				goto next
			case r == '{' || r == '(' || r == '[':
				depth++
			case r == '}' || r == ')' || r == ']':
				depth--
			}
		}
	next:
		quote = 0
	}
	return depth
}

// continueStatement дописывает строку к незакрытой инструкции и сообщает,
// закрылась ли она.
func continueStatement(raw string) (string, bool) {
	shellPending += "\n" + raw
	if blockDepth(shellPending) > 0 {
		return "", false
	}
	src := shellPending
	shellPending = ""
	return src, true
}

// runShellStatement выполняет инструкцию языка в сеансе оболочки.
func runShellStatement(base context.Context, src string) (terminated bool) {
	return runInterruptible(base, func(ctx context.Context) {
		// Права и журнал аудита — как у eval.
		if err := authorize(evalCmd, []string{src}); err != nil {
			shellFail(err)
			return
		}
		file, err := lang.Parse("<shell>", src)
		if err != nil {
			shellFail(err)
			return
		}
		registry, _ := aliases()
		if err := registry.AddFile("", withoutRedeclared(shellDecls, file)); err != nil {
			shellFail(err)
			return
		}
		if err := registry.AddFile("", file); err != nil {
			shellFail(err)
			return
		}
		interp := lang.NewInterpreter(ctx, registry, os.Stdout)
		interp.SetErr(os.Stderr)
		interp.SetProtected(protectedPaths())
		interp.SetCommands(scriptCommands{})
		value, err := interp.RunLine(shellSession, file)
		shellDecls = withoutRedeclared(shellDecls, file)
		shellDecls.Imports = append(shellDecls.Imports, file.Imports...)
		shellDecls.Aliases = append(shellDecls.Aliases, file.Aliases...)
		shellDecls.Records = append(shellDecls.Records, file.Records...)
		shellDecls.Interfaces = append(shellDecls.Interfaces, file.Interfaces...)
		if err != nil {
			shellFail(err)
			return
		}
		// Значение строки оболочка показывает, как любой REPL; print своё уже
		// напечатал — второй раз то же самое не нужно.
		if _, null := value.(lang.NullValue); !null && !endsWithPrint(file) {
			fmt.Println(lang.Display(value))
		}
	})
}

// endsWithPrint — последняя инструкция строки — вызов print: он возвращает
// то, что напечатал.
func endsWithPrint(file *lang.File) bool {
	if len(file.Stmts) == 0 {
		return false
	}
	stmt, ok := file.Stmts[len(file.Stmts)-1].(*lang.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.Expr.(*lang.Call)
	return ok && call.Receiver == nil && call.Callee != nil && call.Callee.Module == "" && call.Callee.Name == "print"
}

// withoutRedeclared — объявления оболочки без тех, что строка объявляет
// заново: в оболочке алиас поправляют, написав его ещё раз.
func withoutRedeclared(decls, file *lang.File) *lang.File {
	names := map[string]bool{}
	for _, a := range file.Aliases {
		names[a.Name] = true
	}
	for _, r := range file.Records {
		names[r.Name] = true
	}
	for _, i := range file.Interfaces {
		names[i.Name] = true
	}
	out := &lang.File{Name: decls.Name, Imports: decls.Imports}
	for _, a := range decls.Aliases {
		if !names[a.Name] {
			out.Aliases = append(out.Aliases, a)
		}
	}
	for _, r := range decls.Records {
		if !names[r.Name] {
			out.Records = append(out.Records, r)
		}
	}
	for _, i := range decls.Interfaces {
		if !names[i.Name] {
			out.Interfaces = append(out.Interfaces, i)
		}
	}
	return out
}
