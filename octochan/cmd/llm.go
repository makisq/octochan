package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"ochan/embedded"
	"ochan/fp"
	"ochan/rings"
	"ochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Режим модели.
//
//	llm:
//	  mode: embedded            # protocol | embedded | natural
//	  model: ~/models/own       # своя модель ℒ: ochan загружает её в свой процесс
//	  endpoint: http://127.0.0.1:8093   # без llm.model — модель по адресу (ochan llm serve там)
//	  decoder:                  # декодер ochan — для чужого движка
//	    tau: 0.42               # кольцо 0
//	    derive: true            # выводимое пишет логика ochan
//
// protocol — модель внешняя (по MCP), задачи — формальным промптом, кольца
// 1–3 читают её готовый ответ. natural — та же внешняя модель, задачи и
// письма на естественном языке (прежний fp.mode: natural). embedded —
// локальная модель по протоколу движка v1, только формальный промпт:
// natural с ней несовместим.
//
// Подходов два, и делятся они не по месту, а по тому, чья модель. Своя
// модель ℒ (/v1/info: "generate": true) — архитектурная, и режим у неё
// один: логика и кольца в её коде, ответ она пишет целиком сама, ochan
// проверяет его снаружи. Её параметры — rings.json рядом с весами (llm fit,
// llm learn); настройки декодера к ней не применяются. Чужой локальный
// движок (только распределение следующего токена) ведёт декодер ochan —
// это протокольный подход на своей машине: кольцо 0 и маска языка не дают
// ему напечатать токен ниже порога и утверждение вне ℒ.

type llmModeKind string

const (
	llmProtocol llmModeKind = "protocol"
	llmEmbedded llmModeKind = "embedded"
	llmNatural  llmModeKind = "natural"
)

// llmMode — режим из llm.mode, а без него — из fp.mode.
func llmMode() (llmModeKind, error) {
	fpMode, err := fp.ParseMode(viper.GetString("fp.mode"))
	if err != nil {
		return "", err
	}
	text := strings.ToLower(strings.TrimSpace(viper.GetString("llm.mode")))
	if text == "" {
		if fpMode == fp.Natural {
			return llmNatural, nil
		}
		return llmProtocol, nil
	}
	mode := llmModeKind(text)
	switch mode {
	case llmProtocol, llmEmbedded, llmNatural:
	default:
		return "", fmt.Errorf("llm.mode: %q, допустимо protocol, embedded или natural", text)
	}
	if viper.InConfig("fp.mode") && fpMode == fp.Natural && mode != llmNatural {
		if mode == llmEmbedded {
			return "", errors.New("llm.mode: embedded и fp.mode: natural вместе нельзя: встроенная модель знает только формальный промпт")
		}
		return "", fmt.Errorf("llm.mode: %s и fp.mode: natural противоречат друг другу: оставьте llm.mode", mode)
	}
	return mode, nil
}

// protocolMode — каким протоколом говорить с моделью.
func protocolMode(mode llmModeKind) fp.Mode {
	if mode == llmNatural {
		return fp.Natural
	}
	return fp.Formal
}

// Настройки декодера ochan — llm.decoder.*; прежние ключи читаются, пока
// нового нет в конфиге.
var decoderOld = map[string]string{
	"tau": "llm.rings.tau", "max_tokens": "llm.max_tokens", "retries": "llm.retries", "derive": "llm.derive",
}

var decoderNames = []string{"tau", "derive", "max_tokens", "retries"}

var decoderDefaults = map[string]any{"tau": 0.42, "derive": true, "max_tokens": 2048, "retries": 32}

// decoderKey — ключ настройки декодера, который действует.
func decoderKey(name string) string {
	key := "llm.decoder." + name
	if old := decoderOld[name]; !viper.InConfig(key) && viper.IsSet(old) {
		return old
	}
	return key
}

// decoderInConfig — настройки декодера, заданные в конфиге не по
// умолчанию, и прежние ключи среди них. Значение по умолчанию в конфиге —
// то, что записал сам ochan, а не настройка оператора.
func decoderInConfig() (set, old []string) {
	for _, name := range decoderNames {
		switch key := decoderKey(name); {
		case key != "llm.decoder."+name:
			set, old = append(set, key), append(old, key)
		case viper.InConfig(key) && fmt.Sprint(viper.Get(key)) != fmt.Sprint(decoderDefaults[name]):
			set = append(set, key)
		}
	}
	return set, old
}

// llmTau — порог кольца 0 декодера.
func llmTau() (float64, error) {
	key := decoderKey("tau")
	tau, err := fraction(viper.Get(key))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if tau == 0 {
		return 0, fmt.Errorf("%s: 0 — порога нет, кольцо 0 пропускало бы всё", key)
	}
	return tau, nil
}

// modeLine — режим одной строкой: что он гарантирует.
func modeLine(mode llmModeKind) string {
	switch mode {
	case llmEmbedded:
		tau, err := llmTau()
		if err != nil {
			return "embedded (" + err.Error() + ")"
		}
		return fmt.Sprintf("embedded (своя модель — логика и кольца в её коде; чужой движок ведёт декодер ochan: "+
			"кольцо 0, τ=%g — на позицию не больше %d токенов)", tau, rings.MaxPass(tau))
	case llmNatural:
		return "natural (задачи и письма на естественном языке; кольца 0 нет)"
	}
	return "protocol (модель внешняя: кольца 1–3 читают готовый ответ, кольца 0 нет)"
}

// knowledgeImports — пакеты знаний, которые задача подключает: «имя
// версия» установленной версии.
func knowledgeImports(prog *fp.Program, opts fp.Options) []string {
	var out []string
	for _, imp := range prog.Imports {
		if opts.Library == nil {
			break
		}
		if skill, ok := opts.Library(imp.Skill); ok && skill.Knowledge != nil {
			out = append(out, imp.Skill+" "+skill.Version)
		}
	}
	return out
}

// modulesDir — каталог пакетов ochan: из него своя модель читает знания.
func modulesDir() string {
	if fm, err := mustFileManager(); err == nil {
		return fm.ModulesDir()
	}
	return "~/.octochan/modules"
}

// printApproach — подход к модели по тому, что движок сказал о себе: своя
// модель — архитектурная, чужой движок ведёт декодер ochan.
func printApproach(cmd *cobra.Command, info embedded.Info) {
	set, old := decoderInConfig()
	if info.Generate {
		cmd.Println("Подход:  архитектурный — своя модель ℒ: логика и кольца в её коде, ответ она пишет сама, ochan проверяет его снаружи")
		cmd.Println("Кольца:  параметры модели — rings.json рядом с весами (ochan llm fit, ochan llm learn)")
		if len(info.Knowledge) > 0 {
			cmd.Printf("Знания:  @%s — пакеты знаний, которые видит модель (import в задаче)\n", strings.Join(info.Knowledge, ", @"))
		} else {
			cmd.Printf("Знания:  пакетов знаний модель не видит (каталог модулей ochan: %s)\n", modulesDir())
		}
		if len(set) > 0 {
			cmd.Printf("внимание: %s — настройки декодера ochan для чужого движка; своя модель их не читает\n", strings.Join(set, ", "))
		}
	} else if tau, err := llmTau(); err != nil {
		cmd.Printf("Подход:  протокольный — чужой движок ведёт декодер ochan; %v\n", err)
	} else {
		derive := "пишет модель (llm.decoder.derive: false)"
		if viper.GetBool(decoderKey("derive")) {
			derive = "пишет логика ochan"
		}
		cmd.Printf("Подход:  протокольный — чужой движок ведёт декодер ochan: кольцо 0, τ=%g; выводимое из посылок %s\n", tau, derive)
	}
	for _, key := range old {
		for name, was := range decoderOld {
			if was == key {
				cmd.Printf("внимание: %s — прежний ключ, теперь llm.decoder.%s\n", key, name)
			}
		}
	}
}

func generationJournal() (*workspace.Generations, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return workspace.NewGenerations(filepath.Join(fm.BaseDir(), llmDir, "generations.jsonl")), nil
}

const llmDir = "llm"

var llmCmd = &cobra.Command{
	Use:   "llm",
	Short: "Модель: режим, движок встроенной модели, генерация",
	Long: "Режим — llm.mode: protocol (модель внешняя, по MCP), natural (внешняя, на\n" +
		"естественном языке) или embedded (локальная модель по протоколу движка\n" +
		"v1). Своя модель ℒ — архитектурная: логика и кольца в её коде, ответ она\n" +
		"пишет сама. Чужой локальный движок ведёт декодер ochan (llm.decoder.*):\n" +
		"кольцо 0 и маска языка внутри генерации. Без аргументов — режим и, для\n" +
		"embedded, движок: доступен ли, что о себе говорит и какой подход у него.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		mode, err := llmMode()
		if err != nil {
			return err
		}
		cmd.Printf("Режим:   %s\n", modeLine(mode))
		if mode != llmEmbedded {
			cmd.Printf("Язык:    %s\n", mailLanguageLine())
			return nil
		}
		engine, err := llmEngine()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		info, err := engine.Info(ctx)
		if err != nil {
			cmd.Printf("Движок:  %s — %v\n", engine.Where(), err)
			cmd.Println("Модель:  своя на этой машине — llm.model (каталог её весов); на другой машине — ochan llm serve там")
		} else {
			if local, ok := engine.(localBackend); ok {
				cmd.Printf("Модель:  %s — в процессе ochan\n", describeModel(local.Model))
			} else {
				cmd.Printf("Движок:  %s — протокол v%d, %s, модель %s\n", engine.Where(), info.Protocol, info.Engine, info.Model)
				if want := viper.GetString("llm.engine"); want != "" && !strings.EqualFold(want, info.Engine) {
					cmd.Printf("внимание: llm.engine: %s, а по адресу отвечает %s\n", want, info.Engine)
				}
			}
			printApproach(cmd, info)
		}
		cmd.Println("Язык:    только формальный промпт: в ответе модели — утверждения ℒ, ничего на естественном языке")
		return err
	},
}

var (
	llmPrompt string
	llmOut    string
)

var llmGenerateCmd = &cobra.Command{
	Use:   "generate [--prompt задача.fp] [--out файл]",
	Short: "Локальная модель пишет ответ на языке: своя — сама, чужую ведёт декодер ochan",
	Long: "Модель получает протокол и задачу (--prompt — формальный промпт оператора)\n" +
		"и пишет ответ. Сначала ochan сверяет протокол движка (/v1/info): без\n" +
		"подтверждённого протокола генерации нет.\n\n" +
		"Своя модель ℒ — архитектурная: логика и кольца в её коде, ответ на задачу\n" +
		"она пишет целиком сама, а ochan проверяет его снаружи и пишет в журнал\n" +
		"решений. Настройки декодера llm.decoder.* к ней не применяются.\n\n" +
		"Чужой движок ведёт декодер ochan: на каждом шаге ochan берёт у движка\n" +
		"распределение следующего токена, токен ниже порога llm.decoder.tau не\n" +
		"берётся (кольцо 0), комментарий и символы вне ASCII — тоже. Собранная\n" +
		"строка — утверждение — проверяется по языку; не в языке — откат к её\n" +
		"началу, и путь, который увёл из языка, запрещён. Ответ — только принятые\n" +
		"утверждения, и целиком он проходит проверку языка; иначе ответа нет.\n\n" +
		"Задачу не на языке и задачу без предпосылки (⊥undef: требование,\n" +
		"операция, скилл, обобщение, которых нет) модель не получает: ochan сам\n" +
		"возвращает перечень недостающего — догадывать его модели нечем.\n\n" +
		"Ответ — в stdout (или --out), сводка колец — в stderr: ответ можно сразу\n" +
		"отправить, например ochan mail send --to operators -.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		mode, err := llmMode()
		if err != nil {
			return err
		}
		if mode != llmEmbedded {
			return fmt.Errorf("llm generate — для своей модели: llm.mode: embedded (сейчас %s; внешняя модель пишет сама, через MCP)", mode)
		}
		engine, err := llmEngine()
		if err != nil {
			return err
		}
		opts, err := fpOptions(cmd, false)
		if err != nil {
			return err
		}
		prompt := fp.ProtocolFor(fp.Formal)
		var (
			plan *fp.AnswerPlan
			task archTask
		)
		if llmPrompt != "" {
			src, err := readSource(llmPrompt)
			if err != nil {
				return err
			}
			r := fp.CheckText(src.Name, src.Content, opts)
			if !r.InLanguage() {
				return fmt.Errorf("%s: задача не на языке (%s) — модель её не получит", src.Name, countCodes(r.Syntax()))
			}
			// Недостающую предпосылку модель не восполняет (П5, раздел 5.2
			// манифеста): перечень недостающего возвращает сама система, и
			// вопрос уходит оператору. Отказ не зависит от того, выучила ли
			// модель H3.
			var missing []string
			for _, d := range r.Diagnostics {
				if d.Code == fp.ErrUndef {
					missing = append(missing, "  "+d.String())
				}
			}
			if len(missing) > 0 {
				return fmt.Errorf("%s: не хватает предпосылок (⊥undef %d) — модель задачу не получит, вопрос к оператору:\n%s",
					src.Name, len(missing), strings.Join(missing, "\n"))
			}
			prompt += "\n\n# Task\n\n" + src.Content
			plan = fp.PlanAnswer(src.Content, r.Program)
			task = archTask{name: src.Name, text: src.Content, plan: plan, premises: premisesOf(r.Program),
				knowledge: knowledgeImports(r.Program, opts)}
		}
		// Протокол движка — строгий: не подтвердил его — генерации нет.
		info, err := engine.Info(cmd.Context())
		if err != nil {
			return fmt.Errorf("%w — генерации нет: ochan говорит с движком только по подтверждённому протоколу", err)
		}
		// Своя модель — архитектурная, и режим у неё один: отвечает целиком
		// сама, её логика и кольца в её коде, ochan только проверяет ответ.
		if info.Generate {
			if plan == nil {
				return errors.New("своя модель отвечает на задачу: --prompt задача.fp — без задачи ей отвечать не на что")
			}
			if set, _ := decoderInConfig(); len(set) > 0 {
				cmd.PrintErrf("внимание: %s — настройки декодера ochan для чужого движка; своя модель их не читает\n",
					strings.Join(set, ", "))
			}
			// Знания, которые задача подключает, модель должна видеть сама:
			// иначе она ответила бы без них, как будто их нет.
			var lacking []string
			for _, k := range task.knowledge {
				if !slices.Contains(info.Knowledge, k) {
					lacking = append(lacking, "@"+k)
				}
			}
			if len(lacking) > 0 {
				return fmt.Errorf("задача подключает знания %s, а модель их не видит — генерации нет: она читает пакеты "+
					"из каталога модулей ochan (%s)", strings.Join(lacking, ", "), modulesDir())
			}
			return generateArchitectural(cmd, engine, info, prompt, task, opts)
		}
		if len(task.knowledge) > 0 {
			cmd.PrintErrf("внимание: знания @%s — для своей модели; чужой движок их не читает\n", strings.Join(task.knowledge, ", @"))
		}

		tau, err := llmTau()
		if err != nil {
			return err
		}
		// Имена в ответе — из задачи и объектов ochan: открытым именем
		// модель говорила бы мимо языка. Словарь собирается один раз.
		names := modelNames(task.text)
		cfg := embedded.Config{
			Tau:       tau,
			MaxTokens: viper.GetInt(decoderKey("max_tokens")),
			Retries:   viper.GetInt(decoderKey("retries")),
			Statement: func(prefix, statement string) []fp.Diagnostic {
				if bad := fp.StatementSyntax(prefix, statement, opts); len(bad) > 0 {
					return bad
				}
				text := prefix + statement
				return fp.CheckModelText("model", text, opts).ForeignNames(text, names, opts.MaxMeanings)
			},
			Final: func(text string) []fp.Diagnostic {
				r := fp.CheckModelText("model", text, opts)
				if bad := r.Syntax(); len(bad) > 0 {
					return bad
				}
				return r.ForeignNames(text, names, opts.MaxMeanings)
			},
		}
		// Копию задачи пишет ochan, а не модель: это текст оператора. То,
		// что следует из посылок, пишет логика; модель выбирает только там,
		// где посылки оставляют выбор.
		if plan != nil {
			cfg.Prefix = plan.Copy
			if viper.GetBool(decoderKey("derive")) {
				cfg.Packages = plan.Next
			}
		}
		out, genErr := embedded.Generate(cmd.Context(), engine, prompt, cfg)
		var failure *embedded.Failure
		if genErr != nil && !errors.As(genErr, &failure) {
			return genErr
		}
		if failure != nil {
			out = failure.Output
		}
		recordGeneration(cmd, info, tau, out, genErr)
		printGeneration(cmd, tau, out)
		if plan != nil && cfg.Packages != nil {
			for _, q := range plan.Open {
				cmd.PrintErrf("Вопрос оператору: %s у %s — %s; посылки не выбирают между ними\n",
					q.Attr, strings.Join(q.Targets, ", "), strings.Join(q.Values, " или "))
			}
		}
		if genErr != nil {
			return fmt.Errorf("ответа нет: %v", genErr)
		}
		if llmOut != "" {
			if err := os.WriteFile(llmOut, []byte(out.Text), 0o600); err != nil {
				return err
			}
			cmd.PrintErrf("Ответ: %s\n", llmOut)
			return nil
		}
		cmd.Print(out.Text)
		return nil
	},
}

func recordGeneration(cmd *cobra.Command, info embedded.Info, tau float64, out *embedded.Output, genErr error) {
	journal, err := generationJournal()
	if err == nil {
		rec := workspace.Generation{Time: time.Now().UTC(), Role: actor(), Engine: info.Engine, Model: info.Model, Tau: tau,
			OK: genErr == nil, Statements: len(out.Statements), Tokens: out.Tokens, Calls: out.Calls, Ring0: out.Ring0,
			Masked: out.Masked, OutOfLanguage: out.OutOfLanguage, Dead: out.Dead, Rollbacks: out.Rollbacks, EarlyEnd: out.EarlyEnd,
			Forced: out.Forced, Chosen: out.Chosen}
		if genErr != nil {
			rec.Reason = genErr.Error()
		}
		err = journal.Append(rec)
	}
	if err != nil {
		cmd.PrintErrf("внимание: генерация не попала в журнал: %v\n", err)
	}
}

func printGeneration(cmd *cobra.Command, tau float64, out *embedded.Output) {
	cmd.PrintErrf("Утверждений: %d, токенов: %d, обращений к модели: %d\n", len(out.Statements), out.Tokens, out.Calls)
	cmd.PrintErrf("Кольцо 0:    отклонено %d (токенов до порога τ=%g)\n", out.Ring0, tau)
	cmd.PrintErrf("Язык:        утверждений вне ℒ %d, токенов вне языка %d — в ответе их нет\n", out.OutOfLanguage, out.Masked)
	if out.Rollbacks > 0 || out.Dead > 0 {
		cmd.PrintErrf("Откатов:     %d к началу утверждения; позиций без токена выше порога: %d\n", out.Rollbacks, out.Dead)
	}
	if out.Forced > 0 || out.Chosen > 0 {
		cmd.PrintErrf("Логика:      утверждений %d — единственные допустимые; выбрано моделью среди допустимых: %d\n",
			out.Forced, out.Chosen)
	}
}

func init() {
	llmGenerateCmd.Flags().StringVar(&llmPrompt, "prompt", "", "задача — формальный промпт оператора (файл или - для stdin)")
	llmGenerateCmd.Flags().StringVar(&llmOut, "out", "", "записать ответ в файл, а не в stdout")
	llmCmd.AddCommand(llmGenerateCmd)
	rootCmd.AddCommand(llmCmd)
}
