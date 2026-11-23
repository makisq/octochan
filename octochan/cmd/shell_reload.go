package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ochan/core/i18n"
	"ochan/modules/pluginhost"
)

// Две вещи оболочки, ради которых раньше приходилось выходить из неё:
//
//	!!reload                   перечитать конфиг, модули и алиасы, не выходя
//	stats db1 >| db1.txt       записать вывод строки в файл
//	stats db1 >>| all.txt      дописать вывод строки в конец файла
//
// Переменные и алиасы, объявленные в оболочке, !!reload не трогает — их
// сбрасывает !!reset.

// ---------------------------------------------------------------------------
// !!reload
// ---------------------------------------------------------------------------

// shellReload перечитывает всё, что ochan читает при запуске: config.yaml
// (и пути из него), журнал, модули, библиотеку алиасов и язык. Без него
// пересобранный модуль или правку конфига видели только после exit и
// нового входа. Ошибка конфига — отказ: прежние настройки остаются, и
// модули не трогаются.
func shellReload(out io.Writer) error {
	// Изменения модулей, замеченные наблюдателем, перезагрузка учтёт сама.
	drainModuleChanges()

	if err := initConfig(); err != nil {
		return i18n.Errorf("конфиг не перечитан, всё осталось как было: %v", err)
	}
	if err := initLogger(); err != nil {
		return i18n.Errorf("журнал: %v", err)
	}
	fm, err := mustFileManager()
	if err != nil {
		return err
	}

	// Модули — все заново: пересобранный файл с тем же именем наблюдатель
	// видит не всегда (замена переименованием, сетевой диск), а новый
	// процесс модуля — единственный способ получить его новый код.
	mm := pluginhost.GetModuleManager()
	mm.Cleanup()
	loadErr := mm.LoadModulesFromDir(fm.ModulesDir())
	refreshModuleCommands()
	warnings := syncRunners()

	resetAliasCache()
	registry, aliasErrs := aliases()
	applyLanguage()
	refreshShellPrefix()

	fmt.Fprintln(out, i18n.Sprintf("Перечитано: конфиг %s", configPath(fm)))
	var names []string
	commands := 0
	for _, info := range mm.LoadedModules() {
		names = append(names, info.Name)
		commands += info.Commands
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintln(out, i18n.Sprintf("  модулей нет (%s)", fm.ModulesDir()))
	} else {
		fmt.Fprintln(out, i18n.Sprintf("  модули: %s — команд %d", strings.Join(names, ", "), commands))
	}
	fmt.Fprintln(out, i18n.Sprintf("  алиасов: %d", len(registry.List())))
	fmt.Fprintln(out, i18n.T("  переменные и алиасы оболочки сохранены; сбросить — !!reset"))

	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, i18n.Sprintf("внимание: %s", w))
	}
	for _, e := range aliasErrs {
		fmt.Fprintln(os.Stderr, i18n.Sprintf("внимание: алиасы: %v", e))
	}
	if loadErr != nil {
		return i18n.Errorf("часть модулей не загрузилась: %v", loadErr)
	}
	return nil
}

// drainModuleChanges забывает изменения модулей, которые заметил
// наблюдатель: !!reload всё равно загружает каждый модуль заново.
func drainModuleChanges() {
	for {
		select {
		case <-moduleChanges:
		default:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// >| файл
// ---------------------------------------------------------------------------

// Перенаправление вывода строки оболочки в файл, как в bash: «>|» —
// перезаписать файл, «>>|» — дописать в конец. Одиночный «>» в оболочке
// занят языком (сравнение), «>>» — композиция, а «>|» и «>>|» в языке ничего
// не значат — спутать нельзя.
const (
	redirectOperator = ">|"
	appendOperator   = ">>|"
)

// redirection — куда строка пишет вывод: файл и дописывать ли.
type redirection struct {
	target string
	append bool
}

// terminalOut — куда писать то, что видит человек, а не файл: ошибки
// строки, напоминания, итог перенаправления. Пока строка пишет в файл,
// os.Stdout — этот файл.
var terminalOut io.Writer

func shellOut() io.Writer {
	if terminalOut != nil {
		return terminalOut
	}
	return os.Stdout
}

// splitRedirect отделяет «>| файл» или «>>| файл» в конце строки. Внутри
// кавычек и после # оператор — просто текст. r — перенаправление, nil —
// его нет; err — оно записано неправильно: без файла, с двумя файлами, без
// команды, дважды.
func splitRedirect(line string) (command string, r *redirection, err error) {
	at, size := -1, 0
	quote := rune(0)
	runes := []rune(line)
scan:
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			break scan
		case c == '>' && i+2 < len(runes) && runes[i+1] == '>' && runes[i+2] == '|':
			if at < 0 { // второй оператор — ошибка, её назовёт проверка остатка
				at, size = i, 3
			}
			i += 2
		case c == '>' && i+1 < len(runes) && runes[i+1] == '|':
			if at < 0 {
				at, size = i, 2
			}
			i++
		}
	}
	if at < 0 {
		return line, nil, nil
	}
	op := string(runes[at : at+size])
	command = strings.TrimSpace(string(runes[:at]))
	rest := strings.TrimSpace(string(runes[at+size:]))
	if command == "" {
		return "", nil, i18n.Errorf("перед %s нет команды: команда %s файл", op, op)
	}
	if strings.Contains(rest, redirectOperator) {
		return "", nil, i18n.Errorf("перенаправление — одно в строке: вывод пишется в один файл")
	}
	args, err := splitArgs(rest)
	if err != nil {
		return "", nil, err
	}
	switch len(args) {
	case 0:
		return "", nil, i18n.Errorf("после %s нет файла: команда %s файл", op, op)
	case 1:
		return command, &redirection{target: args[0], append: op == appendOperator}, nil
	}
	return "", nil, i18n.Errorf("после %s — один файл, а их %d: путь с пробелами берут в кавычки", op, len(args))
}

// withRedirect выполняет run, направив вывод в файл r.target — с начала
// или в конец (r.append): всё, что строка печатает в stdout, — команды
// ochan, модулей, инструкции языка.
// Ошибки остаются на экране, как в bash у «>»: по пустому файлу не понять,
// что команда не прошла.
//
// Запись в файл — то же, что tee: то же право (группа apply), та же
// область роли и тот же запрет на рабочий каталог ochan, запись в журнал
// аудита. Иначе роль только для чтения писала бы файлы перенаправлением.
func withRedirect(ctx context.Context, r *redirection, run func()) {
	target := r.target
	if err := authorize(teeCmd, []string{target}); err != nil {
		shellFail(err)
		return
	}
	path := expandHome(target)
	mode := os.O_TRUNC
	before := int64(0)
	if r.append {
		mode = os.O_APPEND
		if info, err := os.Stat(path); err == nil {
			before = info.Size()
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|mode, 0o644)
	if err != nil {
		shellFail(i18n.Errorf("вывод не перенаправить в %s: %v", target, err))
		return
	}

	saved := os.Stdout
	// Вывод команд, заданный явно (не os.Stdout), возвращается как был;
	// os.Stdout — пустым значением: cobra берёт тот os.Stdout, что сейчас.
	var savedOut io.Writer
	if out := rootCmd.OutOrStdout(); out != io.Writer(saved) {
		savedOut = out
	}
	terminalOut, os.Stdout = saved, file
	rootCmd.SetOut(file)
	defer func() {
		os.Stdout, terminalOut = saved, nil
		rootCmd.SetOut(savedOut)
	}()

	run()

	if err := file.Close(); err != nil {
		shellFail(i18n.Errorf("вывод в %s не записан до конца: %v", target, err))
		return
	}
	if ctx.Err() != nil {
		return
	}
	size := int64(0)
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	shown := path
	if abs, err := filepath.Abs(path); err == nil {
		shown = abs
	}
	// Итог — человеку: без терминала stdout — вывод, итог уходит в stderr.
	note := saved
	if shellLines {
		note = os.Stderr
	}
	if r.append {
		fmt.Fprintln(note, i18n.Sprintf("Вывод дописан в %s: +%d байт, всего %d", shown, size-before, size))
		return
	}
	fmt.Fprintln(note, i18n.Sprintf("Вывод записан в %s: %d байт", shown, size))
}

// errRedirectInBlock — «>|» на строке, которая открывает блок или
// продолжает его, а не закрывает.
func errRedirectInBlock() error {
	return i18n.Errorf("перенаправление ставят после закрывающей скобки блока: } >| файл или } >>| файл")
}
