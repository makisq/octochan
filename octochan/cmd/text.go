package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Текст: поиск, поля и хвост — то, ради чего на сервере держат grep, awk и
// tail, для журналов и вывода команд; построчное сравнение — ochan diff
// (diff.go): у ochan одно сравнение, по смыслу для конфигов и построчно для
// остального. Только читают;
// пути проверяет checkPaths, как у любой команды: роли с областью — только
// в ней, рабочий каталог ochan — никому, кроме master. Вызываются и из
// сценария — ochan("text grep …").

// maxTextLine — самая длинная строка, которую читают текстовые команды.
const maxTextLine = 1 << 20

// pathArgsFrom — аннотация команды: позиционные аргументы до этого номера —
// не пути (шаблон grep), их checkPaths не проверяет; none — путей нет вовсе
// (хосты и адреса у net).
const pathArgsFrom = "ochan.path-args-from"

var textCmd = &cobra.Command{
	Use:   "text",
	Short: "Текст: поиск, поля и хвост файлов",
	Long: "Поиск по шаблону (grep), поля строк (awk, cut) и последние строки (tail)\n" +
		"— для журналов и вывода команд, без внешних программ. Построчное\n" +
		"сравнение — ochan diff: любой текст, кроме конфигов, он сравнивает\n" +
		"построчно. Только читают; вызываются и из сценария: ochan(\"text …\").",
}

// textInputs — файлы команды: шаблоны (*.log) раскрываются; без файлов —
// stdin из конвейера, но не в сценарии: его ввод — не терминал и не
// конвейер.
func textInputs(args []string) ([]string, error) {
	if len(args) == 0 {
		if scriptDepth > 0 || !stdinIsPipe() {
			return nil, errors.New("укажите файл: на stdin ничего не подано")
		}
		return []string{"-"}, nil
	}
	var files []string
	for _, arg := range args {
		if arg == "-" || !strings.ContainsAny(arg, "*?[") {
			files = append(files, arg)
			continue
		}
		matches, err := filepath.Glob(arg)
		if err != nil {
			return nil, fmt.Errorf("шаблон %q: %w", arg, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("под %s нет файлов", arg)
		}
		files = append(files, matches...)
	}
	return files, nil
}

// eachLine передаёт строки файла (или stdin) по одной, с номерами от 1; fn
// возвращает false — дальше не читать.
func eachLine(path string, fn func(n int, line string) bool) error {
	var r io.Reader
	if path == "-" {
		data, err := readStdin()
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	} else {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("не удалось прочитать %s: %w", path, err)
		}
		defer f.Close()
		r = f
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxTextLine)
	for n := 1; sc.Scan(); n++ {
		if !fn(n, sc.Text()) {
			return nil
		}
	}
	if errors.Is(sc.Err(), bufio.ErrTooLong) {
		return fmt.Errorf("%s: строка длиннее %d КиБ", path, maxTextLine>>10)
	}
	return sc.Err()
}

var textGrep struct {
	ignoreCase, invert, number, count, fixed bool
	max                                      int
}

var textGrepCmd = &cobra.Command{
	Use:   "grep <шаблон> [файл...]",
	Short: "Строки, подходящие под шаблон",
	Long: "Шаблон — регулярное выражение RE2: без возвратов, поэтому время поиска\n" +
		"не взрывается на любом шаблоне. Файлов несколько — перед строкой имя\n" +
		"файла; шаблон файлов (*.log) раскрывается. Совпадений нет — пустой вывод,\n" +
		"а не ошибка: в сценарии это пустая строка.",
	Example: "ochan text grep -i 'error|fatal' /var/log/postgresql/*.log\n" +
		"ochan text grep -c timeout app.log\n" +
		"ochan logs rank --min warning | ochan text grep -n replica",
	Args:        cobra.MinimumNArgs(1),
	Annotations: map[string]string{pathArgsFrom: "1"},
	RunE: func(cmd *cobra.Command, args []string) error {
		pattern := args[0]
		if textGrep.fixed {
			pattern = regexp.QuoteMeta(pattern)
		}
		if textGrep.ignoreCase {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("шаблон %q: %w", args[0], err)
		}
		files, err := textInputs(args[1:])
		if err != nil {
			return err
		}
		many, printed := len(files) > 1, 0
		for _, file := range files {
			count := 0
			err := eachLine(file, func(n int, line string) bool {
				if re.MatchString(line) == textGrep.invert {
					return true
				}
				count++
				if textGrep.count {
					return true
				}
				prefix := ""
				if many {
					prefix = file + ":"
				}
				if textGrep.number {
					prefix += strconv.Itoa(n) + ":"
				}
				cmd.Println(prefix + line)
				printed++
				return textGrep.max == 0 || printed < textGrep.max
			})
			if err != nil {
				return err
			}
			if textGrep.count {
				if many {
					cmd.Printf("%s:%d\n", file, count)
				} else {
					cmd.Println(count)
				}
			}
			if textGrep.max > 0 && printed >= textGrep.max {
				break
			}
		}
		return nil
	},
}

var textFields struct {
	fields, delimiter, output, match string
}

var textFieldsCmd = &cobra.Command{
	Use:   "fields [файл...] [-f 1,3-4] [-d разделитель]",
	Short: "Поля строк: что делают awk '{print $1}' и cut",
	Long: "Строка делится на поля пробелами и табуляциями, как у awk, или\n" +
		"разделителем -d, как у cut. -f — номера полей с 1: 1,3-4, 2- (со второго\n" +
		"до конца); без -f — все. --match — только строки, подходящие под\n" +
		"шаблон, как /шаблон/ у awk. Поля выводятся через -s: по умолчанию тот же\n" +
		"разделитель, а без -d — пробел.",
	Example: "ochan text fields -f 1,4 access.log\n" +
		"ochan text fields -d : -f 1,7 /etc/passwd\n" +
		"ochan text fields --match ERROR -f 2- app.log",
	RunE: func(cmd *cobra.Command, args []string) error {
		pick, err := parseFieldList(textFields.fields)
		if err != nil {
			return err
		}
		var re *regexp.Regexp
		if textFields.match != "" {
			if re, err = regexp.Compile(textFields.match); err != nil {
				return fmt.Errorf("--match %q: %w", textFields.match, err)
			}
		}
		sep := textFields.output
		if !cmd.Flags().Changed("output") {
			sep = " "
			if textFields.delimiter != "" {
				sep = textFields.delimiter
			}
		}
		files, err := textInputs(args)
		if err != nil {
			return err
		}
		for _, file := range files {
			err := eachLine(file, func(_ int, line string) bool {
				if re != nil && !re.MatchString(line) {
					return true
				}
				var parts []string
				if textFields.delimiter == "" {
					parts = strings.Fields(line)
				} else {
					parts = strings.Split(line, textFields.delimiter)
				}
				cmd.Println(strings.Join(pick(parts), sep))
				return true
			})
			if err != nil {
				return err
			}
		}
		return nil
	},
}

// parseFieldList разбирает -f: 1,3-4,2- — номера с 1. Пусто — все поля.
func parseFieldList(spec string) (func([]string) []string, error) {
	if strings.TrimSpace(spec) == "" {
		return func(parts []string) []string { return parts }, nil
	}
	type span struct{ from, to int } // to == 0 — до конца
	var spans []span
	bad := fmt.Errorf("-f %q: номера полей с 1 — 1,3-4 или 2-", spec)
	for _, item := range strings.Split(spec, ",") {
		from, to, isRange := strings.Cut(strings.TrimSpace(item), "-")
		a, err := strconv.Atoi(from)
		if err != nil || a < 1 {
			return nil, bad
		}
		s := span{a, a}
		if isRange {
			s.to = 0
			if to != "" {
				if s.to, err = strconv.Atoi(to); err != nil || s.to < a {
					return nil, bad
				}
			}
		}
		spans = append(spans, s)
	}
	return func(parts []string) []string {
		var out []string
		for _, s := range spans {
			to := s.to
			if to == 0 || to > len(parts) {
				to = len(parts)
			}
			for i := s.from; i <= to; i++ {
				out = append(out, parts[i-1])
			}
		}
		return out
	}, nil
}

var textTail struct {
	lines  int
	follow bool
}

var textTailCmd = &cobra.Command{
	Use:   "tail <файл> [-n 10] [-f]",
	Short: "Последние строки файла; -f — следить за новыми",
	Long: "Файл читается с конца, поэтому большой журнал не читается целиком. -f\n" +
		"печатает дописанное, пока не прервут (Ctrl+C); файл усекли или\n" +
		"повернули — читает его сначала. Из сценария -f нельзя: сценарий не\n" +
		"кончился бы.",
	Example: "ochan text tail -n 50 /var/log/postgresql/postgresql.log\n" +
		"ochan text tail -f app.log",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if textTail.lines < 0 {
			return errors.New("-n — число строк, не меньше 0")
		}
		if textTail.follow && scriptDepth > 0 {
			return errors.New("text tail -f из сценария нельзя: сценарий не кончился бы")
		}
		f, err := os.Open(args[0])
		if err != nil {
			return fmt.Errorf("не удалось прочитать %s: %w", args[0], err)
		}
		defer f.Close()
		last, end, err := lastLines(f, textTail.lines)
		if err != nil {
			return err
		}
		cmd.Print(last)
		if !textTail.follow {
			return nil
		}
		return follow(cmd.Context(), cmd.OutOrStdout(), args[0], end)
	},
}

// lastLines — последние n строк файла и его размер: читается с конца
// кусками, пока строк не хватит.
func lastLines(f *os.File, n int) (string, int64, error) {
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	size := info.Size()
	if n == 0 {
		return "", size, nil
	}
	const chunk = 64 << 10
	var tail []byte
	for pos := size; pos > 0; {
		step := int64(chunk)
		if pos < step {
			step = pos
		}
		pos -= step
		buf := make([]byte, step)
		if _, err := f.ReadAt(buf, pos); err != nil && !errors.Is(err, io.EOF) {
			return "", 0, err
		}
		tail = append(buf, tail...)
		// Последний перевод строки в конце файла строку не начинает.
		if bytes.Count(bytes.TrimSuffix(tail, []byte("\n")), []byte("\n")) >= n {
			break
		}
	}
	lines := strings.SplitAfter(string(tail), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, ""), size, nil
}

// follow печатает то, что дописывают в файл, пока контекст жив.
func follow(ctx context.Context, out io.Writer, path string, offset int64) error {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		info, err := os.Stat(path)
		if err != nil {
			continue // повернули, нового ещё нет
		}
		if info.Size() < offset {
			fmt.Fprintf(out, "— %s усечён: читаю сначала\n", path)
			offset = 0
		}
		if info.Size() == offset {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		n, err := io.Copy(out, io.NewSectionReader(f, offset, info.Size()-offset))
		f.Close()
		offset += n
		if err != nil {
			return err
		}
	}
}

func init() {
	f := textGrepCmd.Flags()
	f.BoolVarP(&textGrep.ignoreCase, "ignore-case", "i", false, "без учёта регистра")
	f.BoolVarP(&textGrep.invert, "invert", "v", false, "строки, которые не подходят")
	f.BoolVarP(&textGrep.number, "line-number", "n", false, "номер строки перед ней")
	f.BoolVarP(&textGrep.count, "count", "c", false, "только число совпадений")
	f.BoolVarP(&textGrep.fixed, "fixed", "F", false, "шаблон — строка, а не регулярное выражение")
	f.IntVarP(&textGrep.max, "max", "m", 0, "не больше стольких строк; 0 — все")

	f = textFieldsCmd.Flags()
	f.StringVarP(&textFields.fields, "fields", "f", "", "номера полей с 1: 1,3-4 или 2-")
	f.StringVarP(&textFields.delimiter, "delimiter", "d", "", "разделитель полей; без него — пробелы и табуляции")
	f.StringVarP(&textFields.output, "output", "s", " ", "разделитель полей в выводе")
	f.StringVar(&textFields.match, "match", "", "только строки, подходящие под шаблон")

	textTailCmd.Flags().IntVarP(&textTail.lines, "lines", "n", 10, "сколько последних строк")
	textTailCmd.Flags().BoolVarP(&textTail.follow, "follow", "f", false, "следить за новыми строками")

	textCmd.AddCommand(textGrepCmd, textFieldsCmd, textTailCmd)
	rootCmd.AddCommand(textCmd)
}
