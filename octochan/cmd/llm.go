package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"octochan/embedded"
	"octochan/fp"
	"octochan/rings"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Режим модели.
//
//	llm:
//	  mode: embedded            # protocol | embedded | natural
//	  engine: llama.cpp         # что за адаптером: для отчёта и сверки
//	  endpoint: http://127.0.0.1:8080
//	  model: ./models/llama-3.1-8b-q4.gguf
//	  rings:
//	    tau: 0.42               # кольцо 0
//
// protocol — модель внешняя (по MCP), задачи — формальным промптом, кольца
// 1–3 читают её готовый ответ. natural — та же внешняя модель, задачи и
// письма на естественном языке (прежний fp.mode: natural). embedded —
// своя локальная модель: ochan ведёт генерацию сам, кольцо 0 и маска языка
// не дают ей напечатать токен ниже порога и утверждение вне ℒ. Эта модель
// знает только формальный промпт: natural с ней несовместим.

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

// llmTau — порог кольца 0.
func llmTau() (float64, error) {
	tau, err := fraction(viper.Get("llm.rings.tau"))
	if err != nil {
		return 0, fmt.Errorf("llm.rings.tau: %w", err)
	}
	if tau == 0 {
		return 0, errors.New("llm.rings.tau: 0 — порога нет, кольцо 0 пропускало бы всё")
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
		return fmt.Sprintf("embedded (кольцо 0 активно, τ=%g: на позицию проходит не больше %d токенов)", tau, rings.MaxPass(tau))
	case llmNatural:
		return "natural (задачи и письма на естественном языке; кольца 0 нет)"
	}
	return "protocol (модель внешняя: кольца 1–3 читают готовый ответ, кольца 0 нет)"
}

func llmEngine() (*embedded.HTTP, error) {
	endpoint := strings.TrimSpace(viper.GetString("llm.endpoint"))
	if endpoint == "" {
		return nil, embedded.ErrNoEngine
	}
	timeout := viper.GetDuration("llm.timeout")
	return embedded.NewHTTP(endpoint, timeout), nil
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
		"естественном языке) или embedded (своя локальная модель: ochan ведёт\n" +
		"генерацию сам, кольцо 0 и маска языка внутри неё). Без аргументов —\n" +
		"режим и, для embedded, движок: доступен ли и что о себе говорит.",
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
			cmd.Printf("Движок:  %s — %v\n", engine.Endpoint, err)
		} else {
			cmd.Printf("Движок:  %s — протокол v%d, %s, модель %s\n", engine.Endpoint, info.Protocol, info.Engine, info.Model)
			if want := viper.GetString("llm.engine"); want != "" && !strings.EqualFold(want, info.Engine) {
				cmd.Printf("внимание: llm.engine: %s, а по адресу отвечает %s\n", want, info.Engine)
			}
		}
		if model := viper.GetString("llm.model"); model != "" {
			cmd.Printf("Модель:  %s (её загружает адаптер)\n", model)
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
	Short: "Встроенная модель пишет ответ на языке: кольцо 0 и маска языка внутри генерации",
	Long: "Модель получает протокол и задачу (--prompt — формальный промпт оператора)\n" +
		"и пишет ответ. На каждом шаге ochan берёт у движка распределение\n" +
		"следующего токена: токен ниже порога llm.rings.tau не берётся (кольцо 0),\n" +
		"комментарий и символы вне ASCII — тоже. Собранная строка — утверждение —\n" +
		"проверяется по языку; не в языке — откат к её началу, и путь, который\n" +
		"увёл из языка, запрещён. Ответ — только принятые утверждения, и целиком он\n" +
		"проходит проверку языка; иначе ответа нет.\n\n" +
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
		tau, err := llmTau()
		if err != nil {
			return err
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
		if llmPrompt != "" {
			src, err := readSource(llmPrompt)
			if err != nil {
				return err
			}
			if r := fp.CheckText(src.Name, src.Content, opts); !r.InLanguage() {
				return fmt.Errorf("%s: задача не на языке (%s) — модель её не получит", src.Name, countCodes(r.Syntax()))
			}
			prompt += "\n\n# Task\n\n" + src.Content
		}

		cfg := embedded.Config{
			Tau:       tau,
			MaxTokens: viper.GetInt("llm.max_tokens"),
			Retries:   viper.GetInt("llm.retries"),
			Statement: func(prefix, statement string) []fp.Diagnostic {
				return fp.StatementSyntax(prefix, statement, opts)
			},
			Final: func(text string) []fp.Diagnostic {
				return fp.CheckModelText("model", text, opts).Syntax()
			},
		}
		info, _ := engine.Info(cmd.Context())
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
			Masked: out.Masked, OutOfLanguage: out.OutOfLanguage, Dead: out.Dead, Rollbacks: out.Rollbacks, EarlyEnd: out.EarlyEnd}
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
}

func init() {
	llmGenerateCmd.Flags().StringVar(&llmPrompt, "prompt", "", "задача — формальный промпт оператора (файл или - для stdin)")
	llmGenerateCmd.Flags().StringVar(&llmOut, "out", "", "записать ответ в файл, а не в stdout")
	llmCmd.AddCommand(llmGenerateCmd)
	rootCmd.AddCommand(llmCmd)
}
