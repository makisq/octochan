package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"

	"octochan/i18n"
	"octochan/pluginhost"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Исходные тексты справки хранятся в аннотациях команды и флага: перевод
// делается всегда с русского, и шаблон для переводчика собирается из
// него же, даже когда справка уже переведена.
const (
	sourceShort   = "i18n.short"
	sourceLong    = "i18n.long"
	sourceExample = "i18n.example"
	sourceUse     = "i18n.use"
	sourceUsage   = "i18n.usage"
)

// commandSource возвращает исходный текст поля команды.
func commandSource(c *cobra.Command, key, current string) string {
	if c.Annotations != nil {
		if text, ok := c.Annotations[key]; ok {
			return text
		}
	}
	return current
}

func flagSource(f *pflag.Flag) string {
	if text, ok := f.Annotations[sourceUsage]; ok && len(text) == 1 {
		return text[0]
	}
	return f.Usage
}

// walkCommands обходит дерево команд вместе с флагами.
func walkCommands(root *cobra.Command, each func(*cobra.Command), flag func(*pflag.Flag)) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		each(c)
		c.LocalNonPersistentFlags().VisitAll(flag)
		c.PersistentFlags().VisitAll(flag)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// helpTexts — исходные тексты справки: описания, примеры, строки вызова
// и подсказки флагов.
func helpTexts(root *cobra.Command) []string {
	seen := map[string]bool{}
	var texts []string
	add := func(text string) {
		if text != "" && hasCyrillic(text) && !seen[text] {
			seen[text] = true
			texts = append(texts, text)
		}
	}
	walkCommands(root, func(c *cobra.Command) {
		add(commandSource(c, sourceShort, c.Short))
		add(commandSource(c, sourceLong, c.Long))
		add(commandSource(c, sourceExample, c.Example))
		add(commandSource(c, sourceUse, c.Use))
	}, func(f *pflag.Flag) { add(flagSource(f)) })
	sort.Strings(texts)
	return texts
}

func hasCyrillic(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// localizeCommands переводит справку дерева команд на текущий язык.
//
// Имя команды — первое слово Use — не переводится никогда: по нему cobra
// находит команду, и перевод, поменявший его, сломал бы вызов.
func localizeCommands(root *cobra.Command) {
	walkCommands(root, func(c *cobra.Command) {
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		for _, field := range []struct {
			key string
			ptr *string
		}{{sourceShort, &c.Short}, {sourceLong, &c.Long}, {sourceExample, &c.Example}} {
			source := commandSource(c, field.key, *field.ptr)
			c.Annotations[field.key] = source
			*field.ptr = i18n.T(source)
		}

		use := commandSource(c, sourceUse, c.Use)
		c.Annotations[sourceUse] = use
		if translated := i18n.T(use); firstWord(translated) == firstWord(use) {
			c.Use = translated
		} else {
			c.Use = use
		}
	}, func(f *pflag.Flag) {
		source := flagSource(f)
		if f.Annotations == nil {
			f.Annotations = map[string][]string{}
		}
		f.Annotations[sourceUsage] = []string{source}
		f.Usage = i18n.T(source)
	})
}

func firstWord(s string) string {
	if fields := strings.Fields(s); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// registerModuleLanguages добавляет языки, которые принесли модули.
func registerModuleLanguages() {
	for _, lang := range pluginhost.GetModuleManager().Languages() {
		if err := i18n.Register(i18n.Language{
			Code: lang.Code, Name: lang.Name, Origin: lang.Module, Entries: lang.Entries,
		}); err != nil {
			fmt.Fprint(os.Stderr, i18n.Sprintf("внимание: модуль %s: язык %s не подключён: %v\n", lang.Module, lang.Code, err))
		}
	}
}

// applyLanguage включает язык из конфига. Неизвестный язык — не повод
// не запускаться: ochan говорит по-русски и объясняет, чего не хватает.
func applyLanguage() {
	registerModuleLanguages()
	if code := viper.GetString("lang"); code != "" {
		if err := i18n.Use(code); err != nil {
			// Язык не подключился — значит, говорить на нём нельзя: о
			// причине сказано и по-русски, и по-английски.
			fmt.Fprintf(os.Stderr, "внимание: lang: %v\n", err)
			fmt.Fprintf(os.Stderr, "warning: lang: language %q is not available (%s); "+
				"install its module with ochan !install <module>\n",
				code, strings.Join(languageCodes(), ", "))
		}
	}
	localizeCommands(rootCmd)
}

func languageCodes() []string {
	var codes []string
	for _, lang := range i18n.Languages() {
		codes = append(codes, lang.Code)
	}
	return codes
}

// coverage — доля текстов справки, у которых есть перевод на язык.
func coverage(code string, texts []string) (int, int) {
	done := 0
	for _, text := range texts {
		if _, ok := i18n.Lookup(code, text); ok {
			done++
		}
	}
	return done, len(texts)
}

var langCmd = &cobra.Command{
	Use:   "lang",
	Short: "Показать языки: текущий, встроенные и из модулей",
	Long: "Язык выбирается в config.yaml: lang: ru или lang: eng (en). Другие языки\n" +
		"приносят модули: модуль ставится через !install, после чего его язык\n" +
		"указывается в lang и действует со следующего запуска ochan.\n\n" +
		"Чего нет в переводе, берётся из английского, а чего нет и там — остаётся\n" +
		"по-русски. Шаблон для перевода — lang template.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		texts := helpTexts(rootCmd)
		current := i18n.Current()
		for _, lang := range i18n.Languages() {
			marker := " "
			if lang.Code == current {
				marker = "*"
			}
			origin := i18n.T("встроен")
			if lang.Origin != "" {
				origin = i18n.Sprintf("модуль %s", lang.Origin)
			}
			done, total := coverage(lang.Code, texts)
			cmd.Printf("%s %-4s %-12s %-20s %s\n", marker, lang.Code, lang.Name, origin,
				i18n.Sprintf("справка переведена: %d из %d", done, total))
		}
		cmd.Println(i18n.T("\n* — текущий язык (lang в config.yaml)"))
		return nil
	},
}

var langTemplateCmd = &cobra.Command{
	Use:   "template [язык]",
	Short: "Выгрузить шаблон перевода: русский текст → перевод",
	Long: "Печатает JSON-объект: ключ — русский текст справки или сообщения, значение —\n" +
		"перевод на указанный язык (пусто — перевода ещё нет). Этот объект модуль\n" +
		"языка отдаёт ochan через GetTranslations; см. examples/langmodule.",
	Example: "ochan lang template de > de.json",
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		code := ""
		if len(args) == 1 {
			code = i18n.Normalize(args[0])
		}
		texts := helpTexts(rootCmd)
		texts = append(texts, i18n.Messages()...)
		out := make(map[string]string, len(texts))
		for _, text := range texts {
			translated := ""
			if code != "" && code != i18n.Source {
				translated, _ = i18n.Lookup(code, text)
			}
			out[text] = translated
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		cmd.Println(string(data))
		return nil
	},
}

func init() {
	langCmd.AddCommand(langTemplateCmd)
}
