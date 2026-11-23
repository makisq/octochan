package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"ochan/lang"
	"ochan/rbac"
	"ochan/workspace"

	"github.com/spf13/cobra"
)

// search — найти файл или заявку и посмотреть, что в них, не выходя из
// оболочки: где лежит сценарий, который написала нейросеть, какой план у
// заявки, в каком файле упоминается параметр. Раньше за этим выходили в
// системную оболочку — за ls, find и cat.
//
// Только читает. Пути — как у любой команды: роли с областью видят файлы
// только в ней, рабочий каталог ochan — никто, кроме master.

var (
	searchIn    []string
	searchText  string
	searchShow  bool
	searchList  bool
	searchAll   bool
	searchDepth int
	searchLimit int
)

// searchSkip — каталоги, в которые поиск не заходит: в них не то, что
// искали, а файлов много.
var searchSkip = map[string]bool{"node_modules": true, "vendor": true, "__pycache__": true}

const (
	// searchMaxShow — сколько байт файла показывает search: дальше — tail и grep.
	searchMaxShow = 256 << 10
	// searchMaxScan — сколько байт файла читается при поиске по тексту.
	searchMaxScan = 8 << 20
)

var fileSearchCmd = &cobra.Command{
	Use:   "search [имя | шаблон] [--in папка] [--text слово] [--show | --list]",
	Short: "Найти файлы и заявки и показать, что в них",
	Long: "Ищет в текущей папке и ниже — по имени файла (часть имени или шаблон с *\n" +
		"и ?) и, с --text, по содержимому. Один найденный файл показывает сразу:\n" +
		"так проверяют сценарий, не выходя из оболочки. Несколько — списком, с\n" +
		"размером и временем; --show печатает содержимое каждого, --list — только\n" +
		"список. Без имени — последние изменённые файлы папки.\n\n" +
		"Вместе с файлами ищутся заявки: по номеру, сценарию, цели и описанию —\n" +
		"план заявки лежит в ней самой (ochan requests <номер>). Без имени\n" +
		"показываются заявки, которые ждут решения.\n\n" +
		"--in — другая папка (можно несколько раз). Скрытые каталоги, vendor и\n" +
		"node_modules пропускаются; --all заходит и в скрытые. Только читает; пути\n" +
		"— по области роли и мимо рабочего каталога ochan.",
	Example: "ochan search scenario.yaml\n" +
		"ochan search '*.ochan'\n" +
		"ochan search --text work_mem\n" +
		"ochan search postgres --in /srv/configs --list\n" +
		"ochan search",
	Args:        cobra.MaximumNArgs(1),
	Annotations: map[string]string{pathArgsFrom: "1"},
	RunE: func(cmd *cobra.Command, args []string) error {
		if searchShow && searchList {
			return errors.New("--show и --list вместе: выберите одно")
		}
		pattern := ""
		if len(args) == 1 {
			pattern = strings.TrimSpace(args[0])
		}
		roots := searchIn
		if len(roots) == 0 {
			roots = []string{"."}
		}
		visible, err := searchVisible()
		if err != nil {
			return err
		}
		match, err := searchMatcher(pattern)
		if err != nil {
			return err
		}
		var found []searchFile
		for _, root := range roots {
			files, err := searchWalk(expandHome(root), match, visible)
			if err != nil {
				return err
			}
			found = append(found, files...)
		}
		if searchText != "" {
			found = searchContent(found, searchText)
		}
		// Без имени — что меняли последним; с именем — по порядку путей.
		if pattern == "" && searchText == "" {
			sort.SliceStable(found, func(i, j int) bool { return found[i].time.After(found[j].time) })
		} else {
			sort.SliceStable(found, func(i, j int) bool { return found[i].path < found[j].path })
		}

		requests, err := searchRequests(pattern)
		if err != nil {
			return err
		}
		if len(found) == 0 && len(requests) == 0 {
			what := "файлов и заявок"
			if pattern != "" {
				what = fmt.Sprintf("файлов и заявок с %q", pattern)
			}
			if searchText != "" {
				what += fmt.Sprintf(" и текстом %q", searchText)
			}
			return fmt.Errorf("не найдено: %s в %s нет", what, strings.Join(roots, ", "))
		}

		show := searchShow || len(found) == 1 && !searchList && (pattern != "" || searchText != "")
		shown := found
		if !show && len(shown) > searchLimit {
			shown = shown[:searchLimit]
		}
		for i, file := range shown {
			if !show {
				cmd.Printf("  %-48s %9s  %s%s\n", file.path, sizeText(file.size), file.time.Format("02.01.2006 15:04"), file.hits)
				continue
			}
			if i > 0 {
				cmd.Println()
			}
			cmd.Printf("── %s (%s, %s) ──\n", file.path, sizeText(file.size), file.time.Format("02.01.2006 15:04"))
			text, err := searchRead(file.path)
			if err != nil {
				cmd.Printf("  %v\n", err)
				continue
			}
			cmd.Print(text)
			if !strings.HasSuffix(text, "\n") {
				cmd.Println()
			}
		}
		if rest := len(found) - len(shown); rest > 0 {
			cmd.Printf("  … и ещё %d: уточните имя или --limit %d\n", rest, len(found))
		}
		if len(found) > 1 && !show {
			cmd.Println("Показать файл: search <имя точнее>; все найденные — --show")
		}
		if len(requests) > 0 {
			if len(found) > 0 {
				cmd.Println()
			}
			if pattern == "" {
				cmd.Println("Заявки, которые ждут решения:")
			} else {
				cmd.Println("Заявки:")
			}
			for _, line := range requests {
				cmd.Println("  " + line)
			}
			cmd.Println("План заявки: requests <номер>")
		}
		return nil
	},
}

type searchFile struct {
	path string
	size int64
	time time.Time
	hits string // строки с найденным текстом
}

// searchVisible — какие пути роль видит: master — все, остальные — свою
// область и ничего из рабочего каталога ochan.
func searchVisible() (func(path string) bool, error) {
	store, _, err := loadRoles()
	if err != nil {
		return nil, err
	}
	role, _, err := activeRole(store)
	if err != nil {
		return nil, err
	}
	if role.Name == rbac.Master {
		return func(string) bool { return true }, nil
	}
	scoped := &rbac.Role{Name: role.Name, Paths: scopeOf(role)}
	protected := protectedPaths()
	return func(path string) bool {
		full, err := filepath.Abs(path)
		if err != nil {
			return false
		}
		real, err := lang.RealPath(full)
		if err != nil {
			return false
		}
		for _, dir := range protected {
			if rbac.Inside(dir, real) {
				return false
			}
		}
		return scoped.InScope(real)
	}, nil
}

// searchMatcher — подходит ли файл: пустой шаблон — любой; с * ? [ —
// шаблон имени (или пути, если в нём есть /); иначе — часть имени или
// пути, без различия регистра.
func searchMatcher(pattern string) (func(rel, name string) bool, error) {
	if pattern == "" {
		return func(string, string) bool { return true }, nil
	}
	lower := strings.ToLower(filepath.ToSlash(pattern))
	byPath := strings.Contains(lower, "/")
	if strings.ContainsAny(pattern, "*?[") {
		if _, err := filepath.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("шаблон %q: %w", pattern, err)
		}
		return func(rel, name string) bool {
			target := strings.ToLower(name)
			if byPath {
				target = strings.ToLower(filepath.ToSlash(rel))
			}
			ok, _ := filepath.Match(lower, target)
			return ok
		}, nil
	}
	return func(rel, name string) bool {
		if byPath {
			return strings.Contains(strings.ToLower(filepath.ToSlash(rel)), lower)
		}
		return strings.Contains(strings.ToLower(name), lower)
	}, nil
}

// searchWalk — файлы папки root и ниже, которые подходят и которые роль
// видит. Путь, который называет сам файл, показывается как есть.
func searchWalk(root string, match func(rel, name string) bool, visible func(string) bool) ([]searchFile, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("--in %s: %w", root, err)
	}
	if !info.IsDir() {
		if !visible(root) {
			return nil, nil
		}
		return []searchFile{{path: root, size: info.Size(), time: info.ModTime()}}, nil
	}
	var out []searchFile
	base := strings.Count(filepath.Clean(root), string(filepath.Separator))
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Каталог, который не прочитать, — не повод бросать поиск.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path == root {
				return nil
			}
			hidden := strings.HasPrefix(name, ".")
			deep := strings.Count(filepath.Clean(path), string(filepath.Separator))-base > searchDepth
			if searchSkip[name] || hidden && !searchAll || deep || !visible(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(name, ".") && !searchAll {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if !match(rel, name) || !visible(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		shown := path
		if root == "." {
			shown = rel
		}
		out = append(out, searchFile{path: shown, size: info.Size(), time: info.ModTime()})
		return nil
	})
	return out, err
}

// searchContent оставляет файлы с текстом (без различия регистра) и
// приписывает к каждому первые строки, где он нашёлся.
func searchContent(files []searchFile, text string) []searchFile {
	needle := []byte(strings.ToLower(text))
	var out []searchFile
	for _, file := range files {
		if file.size > searchMaxScan {
			continue
		}
		data, err := os.ReadFile(file.path)
		if err != nil || bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			continue
		}
		var lines []string
		total := 0
		for n, line := range bytes.Split(data, []byte("\n")) {
			if !bytes.Contains(bytes.ToLower(line), needle) {
				continue
			}
			if total++; len(lines) < 3 {
				shown := strings.TrimSpace(string(line))
				if len(shown) > 120 {
					shown = shown[:120] + "…"
				}
				lines = append(lines, fmt.Sprintf("\n      %d: %s", n+1, shown))
			}
		}
		if total == 0 {
			continue
		}
		file.hits = strings.Join(lines, "")
		if total > len(lines) {
			file.hits += fmt.Sprintf("\n      … строк с текстом: %d", total)
		}
		out = append(out, file)
	}
	return out
}

// searchRead — содержимое файла для показа: текст; двоичный файл и слишком
// большой — одной строкой.
func searchRead(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, searchMaxShow+1)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		if info, statErr := f.Stat(); statErr == nil && info.Size() == 0 {
			return "  (файл пуст)\n", nil
		}
		return "", err
	}
	data := buf[:min(n, searchMaxShow)]
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return "  (двоичный файл — не показан)\n", nil
	}
	text := string(data)
	if n > searchMaxShow {
		// Оборванный посреди знака хвост не печатается.
		for len(text) > 0 && !utf8.ValidString(text[len(text)-min(len(text), 4):]) {
			text = text[:len(text)-1]
		}
		text = strings.TrimRight(text, "\n") + fmt.Sprintf("\n  … показано %s; дальше — text tail и text grep\n", sizeText(searchMaxShow))
	}
	return text, nil
}

// searchRequests — заявки, которые подходят под имя: по номеру, сценарию,
// цели, службе и описанию; без имени — те, что ждут решения.
func searchRequests(pattern string) ([]string, error) {
	store, err := requestStore()
	if err != nil {
		return nil, err
	}
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	graph := workspace.NewGraph(all)
	lower := strings.ToLower(strings.Trim(pattern, "*"))
	var out []string
	for _, r := range all {
		if pattern == "" {
			if r.State != workspace.RequestPending {
				continue
			}
		} else if !strings.Contains(strings.ToLower(strings.Join([]string{r.ID, r.Source, r.Target, r.Service, r.Summary}, "\n")), lower) {
			continue
		}
		out = append(out, graph.DescribeIn(r))
	}
	return out, nil
}

// sizeText — размер файла коротко.
func sizeText(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d Б", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f КБ", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
}

func init() {
	fileSearchCmd.Flags().StringArrayVar(&searchIn, "in", nil, "папка, в которой искать; по умолчанию — текущая (можно несколько раз)")
	fileSearchCmd.Flags().StringVar(&searchText, "text", "", "только файлы с этим текстом; показывает строки, где он есть")
	fileSearchCmd.Flags().BoolVar(&searchShow, "show", false, "показать содержимое каждого найденного файла")
	fileSearchCmd.Flags().BoolVar(&searchList, "list", false, "только список, даже если файл найден один")
	fileSearchCmd.Flags().BoolVar(&searchAll, "all", false, "заходить и в скрытые каталоги, показывать скрытые файлы")
	fileSearchCmd.Flags().IntVar(&searchDepth, "depth", 8, "на сколько каталогов вглубь искать")
	fileSearchCmd.Flags().IntVar(&searchLimit, "limit", 30, "сколько файлов показывать в списке")
	rootCmd.AddCommand(fileSearchCmd)
}
