package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"octochan/arbiter"
	"octochan/fp"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	fpName         string
	fpStandalone   bool
	fpProtocolMode string
)

var fpCmd = &cobra.Command{
	Use:   "fp",
	Short: "Формальный промпт: проверка и протокол",
	Long: "Формальный промпт — язык, на котором оператор описывает задачу модели.\n" +
		"Проверки выполняются до модели и детерминированы: текст принадлежит языку,\n" +
		"слова однозначны, знания хватает, жёсткие правила совместны.",
}

var fpCheckCmd = &cobra.Command{
	Use:   "check <файл.fp | ->",
	Short: "Проверить формальный промпт",
	Long: "Отказы — по обозначениям манифеста:\n" +
		"  ⊥syn       текст не принадлежит языку\n" +
		"  ⊥ambig     слово может означать несколько классов, и место его не выбирает\n" +
		"  ⊥undef     не хватает отношения, класса, операции или скилла\n" +
		"  ⊥conflict  противоречие: правила, приоритеты, повтор, отчёт против промпта\n\n" +
		"Скиллы из import ищутся среди пакетов алиасов ochan. С --standalone\n" +
		"скилл даёт ровно то, что объявлено в его provides:.\n\n" +
		"Настройки — в config.yaml: fp.max_meanings, fp.consistency, fp.quiet.",
	Args: cobra.ExactArgs(1),
	Example: "ochan fp check задача.fp\n" +
		"cat задача.fp | ochan fp check --name задача.fp -",
	RunE: func(cmd *cobra.Command, args []string) error {
		src, err := readSource(args[0])
		if err != nil {
			return err
		}
		if fpName != "" {
			if args[0] != "-" {
				return fmt.Errorf("--name задаёт имя промпта, поданного на stdin; у файла имя уже есть")
			}
			src.Name = fpName
		}

		opts, err := fpOptions(cmd, fpStandalone)
		if err != nil {
			return err
		}
		result := fp.CheckText(src.Name, src.Content, opts)

		for _, d := range result.Diagnostics {
			cmd.Printf("%s:%s\n", src.Name, d)
		}
		if n := result.Errors(); n > 0 {
			return fmt.Errorf("%s: отказов %d (%s)", src.Name, n, countCodes(result.Diagnostics))
		}

		cmd.Printf("%s: P ∈ ℒ, слова однозначны, Req(P) ⊆ K, жёсткие правила совместны\n", src.Name)
		cmd.Printf("Цель γ:  %s\n", result.Goal)
		cmd.Printf("K:       %s\n", orNone(result.Knowledge))
		cmd.Printf("Req(P):  %s\n", orNone(result.Required))
		for _, name := range sortedPaths(result.Skills) {
			cmd.Printf("Скилл:   @%s %s\n", name, versionOrNone(result.Skills[name]))
		}
		for _, cell := range result.Program.Cells {
			cmd.Printf("D(%s):   %s\n", cell.Name, orNone(result.Dependents[cell.Name]))
		}
		return nil
	},
}

var fpProtocolCmd = &cobra.Command{
	Use:   "protocol",
	Short: "Показать протокол — то, что модель читает первым",
	Long: "Протокол зависит от fp.mode в config.yaml. formal — задачи только\n" +
		"формальным промптом, текст вне языка — отказ ⊥syn. natural — модель\n" +
		"принимает задачи на естественном языке; согласование заявок и пределы\n" +
		"роли остаются, а жёстких правил, по которым проверить план, нет.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		text := viper.GetString("fp.mode")
		if cmd.Flags().Changed("mode") {
			text = fpProtocolMode
		}
		mode, err := fp.ParseMode(text)
		if err != nil {
			return err
		}
		cmd.Print(fp.ProtocolFor(mode))
		return nil
	},
}

// fpOptions собирает настройки проверки: то, что задаётся в конфигурации,
// а не в промпте, и библиотеку скиллов. standalone — без библиотеки:
// скилл даёт то, что объявлено в его provides:.
func fpOptions(cmd *cobra.Command, standalone bool) (fp.Options, error) {
	opts := fp.Options{
		MaxMeanings: viper.GetInt("fp.max_meanings"),
		Consistency: fp.Consistency(viper.GetString("fp.consistency")),
		Quiet:       viper.GetStringSlice("fp.quiet"),
	}
	switch opts.Consistency {
	case "", fp.Strict, fp.Reconcile:
	default:
		return opts, fmt.Errorf("fp.consistency: %q, допустимо strict или reconcile", opts.Consistency)
	}
	lexicon, err := fpLexicon()
	if err != nil {
		return opts, err
	}
	opts.Lexicon = lexicon

	if standalone {
		return opts, nil
	}
	// write и save — операции самого ochan: правило о них арбитр
	// проверяет по плану сценария, и скилл для них не нужен.
	opts.Builtins = arbiter.Builtins

	// Скилл — пакет алиасов: его операции — алиасы пакета. Проверка не
	// верит объявлению в import на слово, а смотрит в библиотеку.
	registry, errs := aliases()
	reportAliasErrors(cmd, errs)
	packages := map[string]*fp.Skill{}
	for _, info := range registry.List() {
		if info.Module == "" {
			continue
		}
		skill, ok := packages[info.Module]
		if !ok {
			skill = &fp.Skill{}
			if pkg, ok := registry.Package(info.Module); ok {
				skill.Version = pkg.Version
			}
			skill.Lexicon, skill.LexiconErr = packageLexicon(info.Module)
			packages[info.Module] = skill
		}
		skill.Ops = append(skill.Ops, info.Name)
	}
	opts.Library = func(name string) (*fp.Skill, bool) {
		skill, ok := packages[name]
		return skill, ok
	}
	opts.Skills = func() []string { return sortedPaths(packages) }
	return opts, nil
}

// fpLexicon — словарь языка вне скиллов: базовый и словари оператора из
// fp.lexicons.
func fpLexicon() (*fp.Lexicon, error) {
	lexicon := fp.BaseLexicon()
	for _, path := range viper.GetStringSlice("fp.lexicons") {
		if rest, ok := strings.CutPrefix(path, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			path = filepath.Join(home, rest)
		}
		add, err := fp.LoadLexicon(path)
		if err != nil {
			return nil, fmt.Errorf("config.yaml fp.lexicons: %w", err)
		}
		if err := lexicon.Merge(add); err != nil {
			return nil, fmt.Errorf("config.yaml fp.lexicons: %s: %w", path, err)
		}
	}
	return lexicon, nil
}

// packageLexicon — словарь пакета алиасов: lexicon.yaml в его каталоге.
// Нет словаря — пакет слов не приносит, и это не ошибка.
func packageLexicon(module string) (*fp.Lexicon, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(fm.ModulesDir(), module, fp.LexiconName)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	lexicon, err := fp.LoadLexicon(path)
	if err != nil {
		return nil, err
	}
	return lexicon.Labeled("@" + module), nil
}

func countCodes(diags []fp.Diagnostic) string {
	counts := map[fp.Code]int{}
	for _, d := range diags {
		if d.IsError() {
			counts[d.Code]++
		}
	}
	var parts []string
	for _, code := range []fp.Code{fp.ErrSyn, fp.ErrAmbig, fp.ErrUndef, fp.ErrConflict} {
		if counts[code] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", code, counts[code]))
		}
	}
	return strings.Join(parts, ", ")
}

func orNone(items []string) string {
	if len(items) == 0 {
		return "—"
	}
	return strings.Join(items, ", ")
}

func init() {
	fpCheckCmd.Flags().StringVar(&fpName, "name", "", "имя промпта, поданного на stdin")
	fpCheckCmd.Flags().BoolVar(&fpStandalone, "standalone", false,
		"не сверять скиллы с библиотекой: скилл даёт то, что объявлено в provides:")
	fpProtocolCmd.Flags().StringVar(&fpProtocolMode, "mode", "",
		"показать протокол режима formal или natural, а не того, что в fp.mode")
	fpCmd.AddCommand(fpCheckCmd, fpProtocolCmd)
}
