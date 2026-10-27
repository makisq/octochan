package cmd

import (
	"fmt"
	"sort"
	"strings"

	"octochan/fp"

	"github.com/spf13/cobra"
)

var fpWordsSkill string

var fpWordsCmd = &cobra.Command{
	Use:   "words [слово...]",
	Short: "Словарь языка: слова, их смыслы и откуда они",
	Long: "Слово со смыслом в формальном промпте — только из словаря. Словарь\n" +
		"складывается из базового, словарей оператора (fp.lexicons) и словарей\n" +
		"скиллов, которые промпт подключил через import. У слова ровно n смыслов,\n" +
		"n < nl — смыслов у него в естественном языке; слово, у которого смыслов\n" +
		"больше потолка fp.max_meanings, в язык не входит.\n\n" +
		"Без аргументов — словарь без скиллов; --skill — словарь скилла; со\n" +
		"словами — их статьи во всех словарях.",
	Example: "ochan fp words\n" +
		"ochan fp words --skill pgconf\n" +
		"ochan fp words rock limit",
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := fpOptions(cmd, false)
		if err != nil {
			return err
		}
		k := opts.MaxMeanings
		if k <= 0 {
			k = 3
		}

		if fpWordsSkill != "" {
			name := strings.TrimPrefix(fpWordsSkill, "@")
			skill, ok := opts.Library(name)
			switch {
			case !ok:
				return fmt.Errorf("скилла @%s нет в библиотеке", name)
			case skill.LexiconErr != nil:
				return fmt.Errorf("словарь скилла @%s негоден: %w", name, skill.LexiconErr)
			case skill.Lexicon == nil:
				cmd.Printf("Скилл @%s слов не приносит: в пакете нет %s.\n", name, fp.LexiconName)
				return nil
			}
			if len(args) == 0 {
				printLexicon(cmd, skill.Lexicon, k)
				return nil
			}
			return printEntries(cmd, args, map[string]*fp.Lexicon{"@" + name: skill.Lexicon}, k)
		}

		if len(args) == 0 {
			printLexicon(cmd, opts.Lexicon, k)
			return nil
		}
		lexicons := map[string]*fp.Lexicon{"": opts.Lexicon}
		for _, name := range opts.Skills() {
			if skill, ok := opts.Library(name); ok && skill.Lexicon != nil {
				lexicons["@"+name] = skill.Lexicon
			}
		}
		return printEntries(cmd, args, lexicons, k)
	},
}

// printLexicon печатает словарь: слова со смыслом, служебные, единицы.
func printLexicon(cmd *cobra.Command, lex *fp.Lexicon, k int) {
	words := lex.Words()
	width := 0
	for _, w := range words {
		width = max(width, len(w))
	}
	for _, w := range words {
		e, _ := lex.Lookup(w)
		cmd.Println(entryLine(w, e, width, k))
	}
	if fn := lex.FunctionWords(); len(fn) > 0 {
		cmd.Printf("\nСлужебные: %s\n", strings.Join(fn, ", "))
	}
	if units := lex.Units(); len(units) > 0 {
		cmd.Printf("Единицы:   %s, %%\n", strings.Join(units, ", "))
	}
	cmd.Printf("\nСлов со смыслом %d, в языке при fp.max_meanings = %d — %d.\n", len(words), k, len(lex.WordsWithin(k)))
}

func entryLine(word string, e *fp.Entry, width, k int) string {
	line := fmt.Sprintf("%-*s  %-28s n=%d < nl=%-3d %s", width, word, strings.Join(e.Senses, ", "),
		e.N(), e.NL, strings.Join(e.Sources, ", "))
	if e.NL == 1 {
		line = fmt.Sprintf("%-*s  %-28s n=1 = nl     %s", width, word, strings.Join(e.Senses, ", "), strings.Join(e.Sources, ", "))
	}
	if e.N() > k {
		line += fmt.Sprintf("  (вне языка при fp.max_meanings = %d)", k)
	}
	return strings.TrimRight(line, " ")
}

// printEntries печатает статьи слов из всех словарей, где они есть.
func printEntries(cmd *cobra.Command, words []string, lexicons map[string]*fp.Lexicon, k int) error {
	sources := make([]string, 0, len(lexicons))
	for name := range lexicons {
		sources = append(sources, name)
	}
	sort.Strings(sources)

	width := 0
	for _, word := range words {
		width = max(width, len(word))
	}
	var missing []string
	for _, word := range words {
		found := false
		for _, src := range sources {
			lex := lexicons[src]
			if e, ok := lex.Lookup(word); ok {
				cmd.Println(entryLine(word, e, width, k))
				found = true
			}
			if lex.IsFunction(word) {
				cmd.Printf("%-*s  служебное слово\n", width, word)
				found = true
			}
		}
		if fp.IsKeyword(word) {
			cmd.Printf("%-*s  ключевое слово языка\n", width, word)
			found = true
		}
		if !found {
			missing = append(missing, word)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("нет ни в одном словаре: %s", strings.Join(missing, ", "))
	}
	return nil
}

func init() {
	fpWordsCmd.Flags().StringVar(&fpWordsSkill, "skill", "", "словарь этого скилла (пакета алиасов)")
	fpCmd.AddCommand(fpWordsCmd)
}
