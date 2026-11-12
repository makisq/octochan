package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"ochan/workspace"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// Установка ochan: куда положить программы и где он будет держать своё.
//
// Быстрая — всё в одной папке: программы, рабочий каталог и модули. Она для
// машин, где политика разрешает работать и запускать программы только в
// одной папке: ни ~/.local/bin, ни ~/.octochan там не годятся, а
// переменные окружения приходилось бы задавать в каждом окне. Рядом с
// программой остаётся указатель ochan.home — и ochan находит своё сам.
//
// Ручная — каждый путь отдельно: программы, рабочий каталог, модули,
// журналы, отчёты, кеш реестров. Вынесенные каталоги записываются в
// config.yaml (paths.*).
//
// Команда работает до рабочего каталога: ей не нужны ни ~/.octochan, ни
// роли — на машине, где домашний каталог закрыт, иначе её было бы не
// запустить. Вопросы читаются из stdin построчно, так что ответы можно
// подать и конвейером; все они задаются и флагами.

var (
	setupHere    string
	setupBin     string
	setupHome    string
	setupDirs    = map[workspace.Dir]*string{}
	setupModules []string
	setupYes     bool
	setupDesktop string
)

// setupDirFlags — флаги вынесенных каталогов и вопросы о них.
var setupDirFlags = []struct {
	dir                   workspace.Dir
	flag, question, usage string
}{
	{workspace.DirModules, "modules", "Модули — там, где разрешён запуск программ",
		"каталог модулей — там, где разрешён запуск программ; по умолчанию — в рабочем каталоге"},
	{workspace.DirLogs, "logs", "Журналы", "каталог журналов; по умолчанию — в рабочем каталоге"},
	{workspace.DirReports, "reports", "Отчёты", "каталог отчётов; по умолчанию — в рабочем каталоге"},
	{workspace.DirRegistryCache, "registry-cache", "Кеш реестров пакетов",
		"кеш реестров пакетов; по умолчанию — в рабочем каталоге"},
}

var setupCmd = &cobra.Command{
	Use:   "setup [--here[=папка]] | [--bin папка] [--home папка] [--modules папка] [--logs папка] [--reports папка] [--registry-cache папка]",
	Short: "Установить ochan: всё в одну папку или каждый путь отдельно",
	Long: "Без флагов спрашивает, как ставить:\n\n" +
		"  1 — быстрая: всё в одной папке — программы ochan и ochan-mcp, рабочий\n" +
		"      каталог (.octochan) и модули. Для машин, где работать и запускать\n" +
		"      программы разрешено только в одной папке.\n" +
		"  2 — ручная: каждый путь задаёте сами; Enter — путь по умолчанию.\n\n" +
		"Ответы читаются из stdin построчно — их можно подать и конвейером. То же\n" +
		"флагами, без вопросов: --here — быстрая установка в текущую папку\n" +
		"(--here=папка — в другую); --bin, --home, --modules, --logs, --reports,\n" +
		"--registry-cache — ручная, незаданное остаётся по умолчанию.\n\n" +
		"Рабочий каталог не в ~/.octochan записывается указателем ochan.home рядом\n" +
		"с программой: ochan находит его сам, переменные окружения не нужны (при\n" +
		"быстрой установке путь в указателе относительный — папку можно переносить\n" +
		"целиком). Вынесенные каталоги узла записываются в config.yaml (paths.*).\n" +
		"Что получилось — ochan paths.\n\n" +
		"--desktop — ещё и значок: запись в меню приложений и ярлык на рабочем\n" +
		"столе, ochan находится поиском системы и открывается в своей оболочке\n" +
		"(Linux и Windows). --desktop=only — только значок для уже установленной\n" +
		"программы, --desktop=remove — убрать его.\n\n" +
		"Команда не требует ни рабочего каталога, ни ролей: её запускают там, где\n" +
		"ochan ещё не установлен.",
	Example: "ochan setup\n" +
		"ochan setup --here --desktop\n" +
		"ochan setup --bin ~/tools --home /srv/ochan --modules /opt/ochan/modules\n" +
		"printf '1\\n' | ochan setup",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error { return runSetup(cmd) },
}

func init() {
	f := setupCmd.Flags()
	f.StringVar(&setupHere, "here", "", "быстрая установка: всё в одной папке (без значения — в текущей)")
	f.Lookup("here").NoOptDefVal = "."
	f.StringVar(&setupBin, "bin", "", "каталог программ ochan и ochan-mcp")
	f.StringVar(&setupHome, "home", "", "рабочий каталог: роли, заявки, журнал аудита, конфиг")
	for _, d := range setupDirFlags {
		setupDirs[d.dir] = f.String(d.flag, "", d.usage)
	}
	f.StringArrayVar(&setupModules, "module", nil, "после установки поставить модуль из файла (можно несколько раз)")
	f.BoolVarP(&setupYes, "yes", "y", false, "не спрашивать подтверждения")
	f.StringVar(&setupDesktop, "desktop", "", "значок в меню приложений и на рабочем столе; only — только значок, remove — убрать")
	f.Lookup("desktop").NoOptDefVal = "yes"
	rootCmd.AddCommand(setupCmd)
}

// setupStandalone — ochan setup до рабочего каталога и ролей. Возвращает
// код выхода; ok — это была команда setup.
func setupStandalone(args []string) (code int, ok bool) {
	if len(args) == 0 || args[0] != "setup" {
		return 0, false
	}
	setupCmd.SetOut(os.Stdout)
	setupCmd.SetErr(os.Stderr)
	setupCmd.InitDefaultHelpFlag()
	if err := setupCmd.ParseFlags(args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
		return 2, true
	}
	if help, _ := setupCmd.Flags().GetBool("help"); help {
		_ = setupCmd.Help()
		return 0, true
	}
	if rest := setupCmd.Flags().Args(); len(rest) > 0 {
		fmt.Fprintf(os.Stderr, "ошибка: лишний аргумент %q — пути задаются флагами (ochan setup --help)\n", rest[0])
		return 2, true
	}
	if err := runSetup(setupCmd); err != nil {
		fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
		return 1, true
	}
	return 0, true
}

// setupWorkDir — подпапка быстрой установки для файлов, с которыми
// работает нейросеть.
const setupWorkDir = "work"

// setupPlan — что и куда ставится.
type setupPlan struct {
	here    bool
	bin     string
	home    string
	dirs    map[workspace.Dir]string // вынесенные каталоги узла; нет — в рабочем
	desktop bool                     // значок в меню приложений и на рабочем столе
}

// desktopSupported — значок на этой системе ставится.
func desktopSupported() bool {
	switch runtime.GOOS {
	case "windows", "linux", "freebsd", "openbsd", "netbsd":
		return true
	}
	return false
}

// defaultBin — каталог программ по умолчанию: тот же, что у scripts/install.
func defaultBin() string {
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "Programs", "ochan")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin")
}

func defaultHome() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".octochan")
}

// defaultDir — где каталог узла лежит, пока его не вынесли.
func defaultDir(home string, d workspace.Dir) string {
	if d == workspace.DirRegistryCache {
		return filepath.Join(home, "registry", "cache")
	}
	return filepath.Join(home, string(d))
}

// setupPath — путь из ответа или флага: ~ и относительный — полным.
func setupPath(value string) (string, error) {
	value = expandHome(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	return filepath.Abs(value)
}

func runSetup(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	in := bufio.NewReader(cmd.InOrStdin())
	// С терминала ответ виден сам; из конвейера и файла он печатается —
	// иначе в журнале установки остались бы одни вопросы.
	typed := cmd.InOrStdin() == os.Stdin && term.IsTerminal(int(os.Stdin.Fd()))
	// ask — вопрос и строка ответа; пустой ответ и конец ввода — def.
	ask := func(question, def string) (answer string, eof bool) {
		if def != "" {
			fmt.Fprintf(out, "%s [%s]: ", question, def)
		} else {
			fmt.Fprintf(out, "%s: ", question)
		}
		line, err := in.ReadString('\n')
		line = strings.TrimSpace(strings.TrimPrefix(line, string(rune(0xFEFF))))
		eof = err != nil && line == ""
		if !typed || eof {
			fmt.Fprintln(out, line)
		}
		if line == "" {
			return def, eof
		}
		return line, false
	}

	switch setupDesktop {
	case "", "yes":
	case "only":
		// Значок для программы, которая уже стоит: той, что запущена.
		self, err := os.Executable()
		if err != nil {
			return err
		}
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		return installDesktop(out, self, desktopWorkdir(filepath.Dir(self)))
	case "remove":
		return removeDesktop(out)
	default:
		return fmt.Errorf("--desktop=%s: без значения — значок вместе с установкой, only — только значок, remove — убрать", setupDesktop)
	}
	plan := setupPlan{dirs: map[workspace.Dir]string{}, desktop: setupDesktop == "yes"}
	manual := setupBin != "" || setupHome != ""
	for _, d := range setupDirFlags {
		manual = manual || *setupDirs[d.dir] != ""
	}
	// asked — ручная установка вопросами; dialog — способ выбран в разговоре.
	asked, dialog := false, false
	here := setupHere
	switch {
	case here != "" && manual:
		return errors.New("--here — всё в одной папке; с ним не задают --bin, --home и каталоги узла")
	case here != "":
		plan.here = true
	case !manual:
		cwd, _ := os.Getwd()
		fmt.Fprintf(out, "Установка ochan. Как ставить?\n"+
			"  1 — быстрая: всё в этой папке — %s\n"+
			"      программы, рабочий каталог (.octochan) и модули; ochan находит своё сам\n"+
			"  2 — ручная: каждый путь задаёте сами, Enter — путь по умолчанию\n", cwd)
		for choice := ""; choice == ""; {
			answer, eof := ask("Выбор, 1 или 2", "")
			switch {
			case answer == "1":
				plan.here, here, dialog, choice = true, ".", true, answer
			case answer == "2":
				asked, dialog, choice = true, true, answer
			case eof:
				// Ответить некому — как раньше: обычные каталоги.
				fmt.Fprintln(out, "Ответа нет — установка в каталоги по умолчанию")
				choice = "-"
			default:
				fmt.Fprintln(out, "  1 — быстрая, 2 — ручная")
			}
		}
	}

	var err error
	if plan.here {
		dir, err := setupPath(here)
		if err != nil {
			return err
		}
		plan.bin, plan.home = dir, filepath.Join(dir, ".octochan")
	} else {
		value := func(question, flag, def string) (string, error) {
			if flag == "" && asked {
				flag, _ = ask(question, def)
			}
			if flag == "" {
				return def, nil
			}
			return setupPath(flag)
		}
		if plan.bin, err = value("Программы ochan и ochan-mcp", setupBin, defaultBin()); err != nil {
			return err
		}
		if plan.home, err = value("Рабочий каталог — роли, заявки, журнал аудита, конфиг", setupHome, defaultHome()); err != nil {
			return err
		}
		for _, d := range setupDirFlags {
			def := defaultDir(plan.home, d.dir)
			path, err := value(d.question, *setupDirs[d.dir], def)
			if err != nil {
				return err
			}
			if path != def {
				plan.dirs[d.dir] = path
			}
		}
	}

	fmt.Fprintf(out, "\nПрограммы:        %s\nРабочий каталог:  %s\n", plan.bin, plan.home)
	for _, d := range setupDirFlags {
		path, where := plan.dirs[d.dir], ""
		if path == "" {
			path, where = defaultDir(plan.home, d.dir), " (в рабочем каталоге)"
		}
		fmt.Fprintf(out, "%-17s %s%s\n", dirNames[d.dir]+":", path, where)
	}
	if dialog && !plan.desktop && desktopSupported() {
		answer, _ := ask("Значок в меню приложений и на рабочем столе? да или нет", "нет")
		plan.desktop = isYes(answer)
	}
	if dialog && !setupYes {
		if answer, _ := ask("Установить? да или нет", "да"); !isYes(answer) {
			return errors.New("установка отменена")
		}
	}
	// Окружение сильнее указателя и config.yaml: с ним установка не та,
	// что записана.
	for _, name := range workspace.PathEnv() {
		if value := os.Getenv(name); value != "" {
			fmt.Fprintf(out, "Внимание: в окружении задано %s=%s — оно сильнее этой установки; уберите его (export из профиля), "+
				"и ochan возьмёт записанные пути\n", name, value)
		}
	}
	return plan.install(cmd)
}

func isYes(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "да", "д", "y", "yes", "1":
		return true
	}
	return false
}

// programs — ochan и ochan-mcp рядом с запущенной программой: что ставить.
func setupPrograms() (dir string, names []string, err error) {
	if dir, err = workspace.ProgramDir(); err != nil {
		return "", nil, err
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	self, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	// Сама программа ставится под именем ochan, как бы её ни назвали.
	names = []string{filepath.Base(self)}
	if _, err := os.Stat(filepath.Join(dir, "ochan-mcp"+ext)); err == nil {
		names = append(names, "ochan-mcp"+ext)
	}
	return dir, names, nil
}

func (p setupPlan) install(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	from, names, err := setupPrograms()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.bin, 0o755); err != nil {
		return fmt.Errorf("каталог программ: %w — выберите папку, куда вам можно писать", err)
	}
	for i, name := range names {
		target := name
		if i == 0 {
			target = "ochan" + ext
		}
		src, dst := filepath.Join(from, name), filepath.Join(p.bin, target)
		if same, _ := sameFile(src, dst); same {
			fmt.Fprintf(out, "На месте: %s\n", dst)
			continue
		}
		if err := installProgram(src, dst); err != nil {
			return err
		}
		fmt.Fprintf(out, "Установлено: %s\n", dst)
	}
	if len(names) < 2 {
		fmt.Fprintf(out, "ochan-mcp рядом с программой нет — сервер MCP не поставлен (make build собирает оба)\n")
	}

	if err := os.MkdirAll(p.home, 0o700); err != nil {
		return fmt.Errorf("рабочий каталог: %w", err)
	}
	// Указатель — когда рабочий каталог не там, где ochan ищет его сам.
	pointer := filepath.Join(p.bin, workspace.HomeFile)
	switch {
	case p.here:
		if err := writeHomeFile(pointer, ".octochan"); err != nil {
			return err
		}
		fmt.Fprintf(out, "Указатель: %s → .octochan (путь от папки программы: её можно переносить целиком)\n", pointer)
	case p.home != defaultHome():
		if err := writeHomeFile(pointer, p.home); err != nil {
			return err
		}
		fmt.Fprintf(out, "Указатель: %s → %s\n", pointer, p.home)
	default:
		if err := os.Remove(pointer); err == nil {
			fmt.Fprintf(out, "Прежний указатель %s убран: рабочий каталог — по умолчанию\n", pointer)
		}
	}
	// Папка программы содержит рабочий каталог — нейросети её не открыть
	// (ask, opmode llm): файлам для модели — своя подпапка.
	work := ""
	if p.here {
		work = filepath.Join(p.bin, setupWorkDir)
		if err := os.MkdirAll(work, 0o755); err != nil {
			return err
		}
	}
	if err := setupConfig(filepath.Join(p.home, "config.yaml"), p.dirs, work != ""); err != nil {
		return err
	}
	if work != "" {
		fmt.Fprintf(out, "Файлы для нейросети: %s — ask и opmode llm читают только её (config.yaml: llm.chat.root: [%s])\n", work, setupWorkDir)
	}
	for _, d := range setupDirFlags {
		if path := p.dirs[d.dir]; path != "" {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("%s: %w", dirNames[d.dir], err)
			}
			fmt.Fprintf(out, "config.yaml: paths.%s: %s\n", d.dir, path)
		}
	}

	// Проверка — установленной программой: она видит то же, что увидит
	// пользователь, и отказывает, если каталог открыт для чужой записи.
	installed := filepath.Join(p.bin, "ochan"+ext)
	env := os.Environ()
	for _, name := range workspace.PathEnv() {
		env = append(env, name+"=")
	}
	env = append(env, "OCTOCHAN_CONFIG=")
	run := func(args ...string) error {
		c := exec.Command(installed, args...)
		c.Env, c.Stdout, c.Stderr = env, out, cmd.ErrOrStderr()
		return c.Run()
	}
	fmt.Fprintln(out)
	if err := run("paths"); err != nil {
		return fmt.Errorf("установленный ochan не запустился: %w", err)
	}
	for _, module := range setupModules {
		path, err := setupPath(module)
		if err != nil {
			return err
		}
		if err := run("!!install", path); err != nil {
			return fmt.Errorf("модуль %s не установился: %w", module, err)
		}
	}

	if p.desktop {
		// Установка в одну папку — там и работать: домашний каталог на такой
		// машине может быть закрыт.
		workdir := desktopWorkdir(p.bin)
		if p.here {
			workdir = p.bin
		}
		if err := installDesktop(out, installed, workdir); err != nil {
			return err
		}
	}

	fmt.Fprintln(out)
	switch {
	case p.here && runtime.GOOS == "windows":
		fmt.Fprintf(out, "Запуск: .\\ochan из папки %s\n", p.bin)
	case p.here:
		fmt.Fprintf(out, "Запуск: ./ochan из папки %s\n", p.bin)
	case !onPath(p.bin):
		fmt.Fprintf(out, "Каталога %s нет в PATH: добавьте его или запускайте %s\n", p.bin, installed)
	default:
		fmt.Fprintln(out, "Запуск: ochan")
	}
	return nil
}

// desktopWorkdir — каталог, в котором значок запускает ochan: домашний, а
// если его нет — папка программы (установка в одну папку).
func desktopWorkdir(bin string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if info, err := os.Stat(home); err == nil && info.IsDir() {
			return home
		}
	}
	return bin
}

func writeHomeFile(path, home string) error {
	text := "# Рабочий каталог ochan: где он держит роли, заявки, журнал аудита и конфиг.\n" +
		"# Записано установкой (ochan setup). Путь без начала от корня — от папки\n" +
		"# программы. Переменная OCTOCHAN_HOME сильнее этого файла.\n" + home + "\n"
	return os.WriteFile(path, []byte(text), 0o644)
}

// setupConfig записывает вынесенные каталоги в config.yaml: прежние
// настройки остаются, paths.* без нового значения — тоже.
func setupConfig(path string, dirs map[workspace.Dir]string, work bool) error {
	if len(dirs) == 0 && !work {
		return nil
	}
	config := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	case !os.IsNotExist(err):
		return err
	}
	paths, _ := config["paths"].(map[string]any)
	if paths == nil {
		paths = map[string]any{}
	}
	for d, dir := range dirs {
		paths[string(d)] = dir
	}
	if len(dirs) > 0 {
		config["paths"] = paths
	}
	if work {
		// Каталог модели — подпапка: путь от папки, из которой запускают
		// ochan. Уже заданный остаётся.
		llm, _ := config["llm"].(map[string]any)
		if llm == nil {
			llm = map[string]any{}
		}
		chat, _ := llm["chat"].(map[string]any)
		if chat == nil {
			chat = map[string]any{}
		}
		if roots, _ := chat["root"].([]any); len(roots) == 0 {
			chat["root"] = []string{setupWorkDir}
		}
		llm["chat"], config["llm"] = chat, llm
	}
	if data, err = yaml.Marshal(config); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// installProgram копирует программу: рядом, затем переименованием — занятая
// программа под Windows не перезаписывается, а отодвигается.
func installProgram(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("%w — выберите папку, куда вам можно писать", err)
	}
	if _, err := io.Copy(file, in); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		old := dst + ".old"
		_ = os.Remove(old)
		if os.Rename(dst, old) != nil || os.Rename(tmp, dst) != nil {
			os.Remove(tmp)
			return fmt.Errorf("не удалось заменить %s: %w", dst, err)
		}
		_ = os.Remove(old)
	}
	return nil
}

func sameFile(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ia, ib), nil
}

// onPath — каталог есть в PATH.
func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == "" {
			continue
		}
		if same, _ := sameFile(p, dir); same {
			return true
		}
	}
	return false
}
