package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ochan/chat"
	"ochan/lang"
	"ochan/rbac"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Разговор с нейросетью из оболочки ochan.
//
//	ochan> ask какие заявки ждут решения?          # одна реплика
//	ochan> opmode llm                              # режим нейросети
//	llm> возьми server1 и server2 из postgres.csv и поставь им work_mem = 32MB
//	llm> ?search work_mem.ochan                    # команда ochan — с «?»
//	llm> ?apply --approve 7f3a…                    # согласовать и выполнить
//	llm> exmode llm                                # обратно
//
// Модель — внешняя (llm.chat.*: Claude, GigaChat, любой сервер с интерфейсом
// OpenAI). ochan ведёт разговор сам и сам вызывает инструменты, о которых
// она просит, — через ochan-mcp: те же границы, что у модели, подключённой
// по MCP. Файлы — только в каталоге, откуда запущена оболочка (llm.chat.root),
// роль — llm, изменения — заявкой, которую согласует человек.
//
// Содержимое файлов, которые модель читает, уходит её поставщику. Поэтому
// право на ask и opmode — своя группа ask: его выдают явно.

// chatKeyName — имя ключа модели в хранилище токенов: ochan auth --executor llm.
const chatKeyName = "llm"

// chatSession — разговор: модель, её инструменты и история.
type chatSession struct {
	conv chat.Conversation
	box  chat.Toolbox
}

// shellChat — режим нейросети в оболочке (opmode llm); nil — обычный режим.
var shellChat *chatSession

// shellActive — команды выполняет оболочка, а не командная строка.
var shellActive bool

// chatDefaults — умолчания llm.chat.*.
var chatDefaults = map[string]any{
	"provider": "", "url": "", "model": "", "key_env": "", "max_tokens": 0, "effort": "", "timeout": "10m", "ca": "",
	"fallbacks": true, "max_steps": 40, "auth_url": "", "scope": "", "mcp": "",
	"root": []string{}, "allow_live": []string{}, "allow_env": []string{}, "allow_net": []string{},
}

// chatKey — ключ модели: хранилище токенов (ochan auth --executor llm),
// переменная из llm.chat.key_env, иначе — принятая у поставщика. У Claude
// без ключа SDK берёт его сам (ANTHROPIC_API_KEY или профиль ant).
func chatKey(provider string) string {
	if fm, err := mustFileManager(); err == nil {
		if key := strings.TrimSpace(fm.ExecutorToken(chatKeyName)); key != "" {
			return key
		}
	}
	names := []string{strings.TrimSpace(viper.GetString("llm.chat.key_env"))}
	switch provider {
	case "openai":
		names = append(names, "OPENAI_API_KEY")
	case "gigachat":
		names = append(names, "GIGACHAT_CREDENTIALS", "GIGACHAT_AUTH_KEY")
	}
	for _, name := range names {
		if name != "" {
			if key := strings.TrimSpace(os.Getenv(name)); key != "" {
				return key
			}
		}
	}
	return ""
}

func chatOptions() (chat.Options, error) {
	provider := strings.ToLower(strings.TrimSpace(viper.GetString("llm.chat.provider")))
	if provider == "" {
		return chat.Options{}, errors.New("модель для разговора не настроена: в config.yaml — llm.chat.provider (" +
			strings.Join(chat.Providers, ", ") + ") и llm.chat.model; ключ — ochan auth --executor llm")
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(viper.GetString("llm.chat.timeout")))
	if err != nil || timeout <= 0 {
		return chat.Options{}, fmt.Errorf("llm.chat.timeout: %q — длительность, например 10m", viper.GetString("llm.chat.timeout"))
	}
	return chat.Options{
		Provider: provider, URL: strings.TrimSpace(viper.GetString("llm.chat.url")), AuthURL: strings.TrimSpace(viper.GetString("llm.chat.auth_url")),
		Scope: strings.TrimSpace(viper.GetString("llm.chat.scope")), Model: strings.TrimSpace(viper.GetString("llm.chat.model")),
		Key: chatKey(provider), MaxTokens: viper.GetInt("llm.chat.max_tokens"), Effort: viper.GetString("llm.chat.effort"),
		Timeout: timeout, CA: expandHome(strings.TrimSpace(viper.GetString("llm.chat.ca"))), Fallbacks: viper.GetBool("llm.chat.fallbacks"),
	}, nil
}

// chatServer — программа ochan-mcp: llm.chat.mcp, рядом с ochan, иначе из PATH.
func chatServer() (string, error) {
	if path := expandHome(strings.TrimSpace(viper.GetString("llm.chat.mcp"))); path != "" {
		return path, nil
	}
	name := "ochan-mcp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if self, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		if path := filepath.Join(filepath.Dir(self), name); fileExists(path) {
			return path, nil
		}
	}
	if path, err := exec.LookPath("ochan-mcp"); err == nil {
		return path, nil
	}
	return "", errors.New("нет ochan-mcp — через него модель вызывает инструменты ochan: он ставится вместе с ochan " +
		"(make install, ochan setup) или называется в llm.chat.mcp")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// chatRoots — каталоги, в которых модель читает файлы и строит планы:
// llm.chat.root (путь без начала от корня — от текущей папки), иначе
// текущая папка. Каталог, в котором лежит рабочий каталог ochan, модели не
// открыть: песочница ядра пустила бы сценарий в него целиком.
func chatRoots() ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, value := range viper.GetStringSlice("llm.chat.root") {
		if value = expandHome(strings.TrimSpace(value)); value != "" {
			if !filepath.IsAbs(value) {
				value = filepath.Join(cwd, value)
			}
			roots = append(roots, filepath.Clean(value))
		}
	}
	configured := len(roots) > 0
	if !configured {
		roots = []string{cwd}
	}
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("llm.chat.root: %s — такой папки нет", root)
		}
		real, err := lang.RealPath(root)
		if err != nil {
			real = root
		}
		for _, dir := range protectedPaths() {
			if !rbac.Inside(real, dir) {
				continue
			}
			if configured {
				return nil, fmt.Errorf("llm.chat.root: в папке %s лежит рабочий каталог ochan (%s) — нейросети она закрыта целиком; "+
					"назовите подпапку с файлами для модели", root, dir)
			}
			return nil, fmt.Errorf("в текущей папке лежит рабочий каталог ochan (%s) — нейросети она закрыта целиком. "+
				"Держите файлы для модели в подпапке и назовите её в config.yaml — llm.chat.root: [%s] — "+
				"или запускайте ochan из такой подпапки", dir, filepath.Join(root, "work"))
		}
	}
	return roots, nil
}

// startChat открывает разговор: сервер инструментов, его протокол —
// системным промптом, его таблица — инструментами модели.
func startChat(ctx context.Context) (*chatSession, error) {
	mode, err := llmMode()
	if err != nil {
		return nil, err
	}
	if mode == llmEmbedded {
		return nil, errors.New("llm.mode: embedded — своя модель знает только формальный промпт: ochan llm generate --prompt задача.fp; " +
			"разговор ведёт внешняя модель — llm.mode: natural и llm.chat.*")
	}
	opts, err := chatOptions()
	if err != nil {
		return nil, err
	}
	server, err := chatServer()
	if err != nil {
		return nil, err
	}
	var args []string
	// Сервер вызывает тот же ochan, что ведёт разговор, а не найденный в PATH.
	if self, err := os.Executable(); err == nil {
		args = append(args, "--ochan", self)
	}
	roots, err := chatRoots()
	if err != nil {
		return nil, err
	}
	for _, root := range roots {
		args = append(args, "--root", root)
	}
	for flag, key := range map[string]string{"--allow-live": "llm.chat.allow_live", "--allow-env": "llm.chat.allow_env",
		"--allow-net": "llm.chat.allow_net"} {
		for _, value := range viper.GetStringSlice(key) {
			if value = strings.TrimSpace(value); value != "" {
				args = append(args, flag, value)
			}
		}
	}
	box, err := chat.StartMCP(ctx, server, args, os.Environ(), "ochan", Version)
	if err != nil {
		return nil, err
	}
	opts.System, opts.Tools = box.Instructions(), box.Tools()
	conv, err := chat.New(opts)
	if err != nil {
		box.Close()
		return nil, err
	}
	return &chatSession{conv: conv, box: box}, nil
}

func (s *chatSession) close() {
	if s != nil && s.box != nil {
		s.box.Close()
	}
}

// send — реплика оператора и ход модели: её текст — в out, вызовы
// инструментов — короткими строками, чтобы было видно, что она делает.
func (s *chatSession) send(ctx context.Context, out io.Writer, text string) error {
	s.conv.Say(text)
	return chat.Turn(ctx, s.conv, s.box, max(viper.GetInt("llm.chat.max_steps"), 1), chat.Events{
		Text: func(text string) { fmt.Fprintln(out, text) },
		Call: func(call chat.Call) { fmt.Fprintf(out, "  → %s %s\n", call.Name, shortJSON(call.Input, 140)) },
		Done: func(_ chat.Call, r chat.Result) {
			if r.IsError {
				fmt.Fprintf(out, "  ← ошибка: %s\n", firstLine(r.Text, 200))
			}
		},
		Other: func(model string) { fmt.Fprintf(out, "  (ответила запасная модель %s)\n", model) },
	})
}

// shortJSON — вход инструмента одной строкой, не длиннее limit знаков.
func shortJSON(raw json.RawMessage, limit int) string {
	var value any
	text := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &value) == nil {
		if compact, err := json.Marshal(value); err == nil {
			text = string(compact)
		}
	}
	return firstLine(text, limit)
}

func firstLine(text string, limit int) string {
	text, _, _ = strings.Cut(strings.TrimSpace(text), "\n")
	if r := []rune(text); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return text
}

// chatNotice — что оператору стоит знать перед разговором.
func chatNotice(out io.Writer, s *chatSession) {
	fmt.Fprintf(out, "Модель: %s; инструментов ochan — %d, роль llm, изменения — заявкой\n", s.conv.Describe(), len(s.box.Tools()))
	if mode, err := llmMode(); err == nil && mode != llmNatural {
		fmt.Fprintln(out, "Режим llm.mode: protocol — модель ждёт задачу формальным промптом; обычным языком — llm.mode: natural")
	}
}

var askCmd = &cobra.Command{
	Use:   "ask <реплика>",
	Short: "Одна реплика нейросети: вопрос или задача, ответ — здесь же",
	Long: "Отправляет реплику внешней модели (llm.chat.* в config.yaml) и печатает\n" +
		"ответ. Это один вызов, без истории; разговор с продолжением — opmode llm\n" +
		"в оболочке. Слова реплики пишутся как есть, кавычки не нужны.\n\n" +
		"Модель работает инструментами ochan — теми же, что у ochan-mcp, и с теми\n" +
		"же границами: читает файлы только в каталоге, откуда вызвана (llm.chat.root\n" +
		"— другие), пишет сценарий, строит план и оформляет заявку; выполнить или\n" +
		"согласовать её она не может — роль llm. Заявку смотрят (search, requests)\n" +
		"и согласуют люди: apply --approve <номер>.\n\n" +
		"Поставщик — llm.chat.provider: anthropic (Claude), gigachat или openai —\n" +
		"любой сервер с тем же интерфейсом, в том числе локальный (llm.chat.url).\n" +
		"Ключ — ochan auth --executor llm; в config.yaml он не попадает. Задачи\n" +
		"обычным языком модель принимает с llm.mode: natural.\n\n" +
		"Содержимое файлов, которые модель читает, уходит её поставщику: право на\n" +
		"ask — отдельная группа ask.",
	Example: "ochan ask какие заявки ждут решения и что в них\n" +
		"ochan ask возьми server1 и server2 из postgres.csv и поставь им work_mem = 32MB",
	Args:        cobra.MinimumNArgs(1),
	Annotations: map[string]string{pathArgsFrom: "none"},
	RunE: func(cmd *cobra.Command, args []string) error {
		text := strings.TrimSpace(strings.Join(args, " "))
		if text == "" {
			return errors.New("пустая реплика")
		}
		session, err := startChat(cmd.Context())
		if err != nil {
			return err
		}
		defer session.close()
		return session.send(cmd.Context(), cmd.OutOrStdout(), text)
	},
}

// modeArg — имя режима из аргументов: «llm», «llm;» — одно и то же.
func modeArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(args[0]), ";"))
}

var opmodeCmd = &cobra.Command{
	Use:   "opmode llm",
	Short: "Режим нейросети в оболочке: каждая строка — реплика модели",
	Long: "Команда оболочки ochan (ochan shell): после opmode llm каждая строка —\n" +
		"реплика внешней модели (llm.chat.*), и разговор продолжается с историей,\n" +
		"пока режим не закрыт. Работает и на терминале, и построчно — из конвейера\n" +
		"или файла.\n\n" +
		"Команды ochan и инструкции языка в этом режиме пишут с «?» в начале:\n" +
		"?search сценарий.ochan, ?requests, ?apply --approve <номер>, ?print(\"x\").\n" +
		"Точка с запятой в конце такой строки не мешает. Выход — exmode llm (можно\n" +
		"и ?exmode llm, и exmode llm;).\n\n" +
		"Что модель может и чего нет, откуда ключ и какой поставщик — ochan ask\n" +
		"--help: opmode — тот же разговор, только с продолжением.",
	Example: "opmode llm\n" +
		"возьми server1 и server2 из postgres.csv и поставь им work_mem = 32MB\n" +
		"?search work_mem\n" +
		"?apply --approve 7f3a2c\n" +
		"exmode llm",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{pathArgsFrom: "none"},
	RunE: func(cmd *cobra.Command, args []string) error {
		if mode := modeArg(args); mode != "llm" {
			return fmt.Errorf("opmode %s: такого режима нет — есть llm", args[0])
		}
		if !shellActive {
			return errors.New("opmode — команда оболочки: ochan shell, затем opmode llm; одной строкой — ochan ask <реплика>")
		}
		if shellChat != nil {
			cmd.Println("Режим нейросети уже включён: выход — exmode llm")
			return nil
		}
		session, err := startChat(cmd.Context())
		if err != nil {
			return err
		}
		shellChat = session
		chatNotice(cmd.OutOrStdout(), session)
		cmd.Println("Режим нейросети: строка — реплика модели; команда ochan — с «?» в начале (?requests, ?apply --approve <номер>); выход — exmode llm")
		return nil
	},
}

var exmodeCmd = &cobra.Command{
	Use:         "exmode [llm]",
	Short:       "Выйти из режима нейросети в оболочке",
	Long:        "Закрывает режим, открытый opmode llm: разговор и его история кончаются,\nстроки оболочки — снова команды ochan.",
	Args:        cobra.MaximumNArgs(1),
	Annotations: map[string]string{pathArgsFrom: "none"},
	RunE: func(cmd *cobra.Command, args []string) error {
		if mode := modeArg(args); mode != "" && mode != "llm" {
			return fmt.Errorf("exmode %s: такого режима нет — есть llm", args[0])
		}
		if shellChat == nil {
			cmd.Println("Режим нейросети не включён")
			return nil
		}
		closeShellChat()
		cmd.Println("Режим нейросети закрыт")
		return nil
	},
}

// closeShellChat закрывает режим нейросети: при exmode, конце ввода и
// выходе из оболочки.
func closeShellChat() {
	if shellChat != nil {
		shellChat.close()
		shellChat = nil
	}
}

// chatModeLine разбирает строку оболочки в режиме нейросети. command —
// строка для обычной оболочки (то, что шло после «?», или exmode); пусто —
// строка была репликой модели и уже обработана.
func chatModeLine(ctx context.Context, input string) (command string) {
	if rest, ok := strings.CutPrefix(input, "?"); ok {
		// ?команда — команда ochan или инструкция языка; ; в конце не мешает.
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), ";"))
	}
	if word, _, _ := strings.Cut(strings.TrimRight(input, "; "), " "); word == "exmode" {
		return strings.TrimRight(input, "; ")
	}
	if terminated := runInterruptible(ctx, func(ctx context.Context) {
		// Права и журнал аудита — как у ask: реплика уходит поставщику модели.
		if err := authorize(askCmd, []string{input}); err != nil {
			shellFail(err)
			return
		}
		if err := shellChat.send(ctx, os.Stdout, input); err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Println("Прервано")
				return
			}
			shellFail(err)
		}
	}); terminated {
		exitShell(143)
	}
	return ""
}

func init() {
	rootCmd.AddCommand(askCmd, opmodeCmd, exmodeCmd)
}
