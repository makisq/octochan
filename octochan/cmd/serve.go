package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"octochan/rbac"
	"octochan/remote"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// EnvRemoteLogin и EnvRemotePassword — логин и пароль для входа на сервер
// без вопросов: для сценариев и CI.
const (
	EnvRemoteLogin    = "OCTOCHAN_REMOTE_LOGIN"
	EnvRemotePassword = "OCTOCHAN_REMOTE_PASSWORD"
)

var (
	serveListen    string
	serveDir       string
	servePlaintext bool
	tlsHosts       []string
	tlsDays        int
	tlsForce       bool
	remoteRole     string
	remotePlain    bool
)

// tlsSettings — параметры ssl и tls из конфига.
func tlsSettings() remote.Settings {
	return remote.Settings{
		SSL:        viper.GetBool("ssl"),
		MinVersion: viper.GetString("tls.min_version"),
		Cert:       viper.GetString("tls.cert"),
		Key:        viper.GetString("tls.key"),
		CA:         viper.GetString("tls.ca"),
		ServerName: viper.GetString("tls.server_name"),
	}
}

// ---------------------------------------------------------------------------
// Сервер
// ---------------------------------------------------------------------------

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Запустить общий оркестратор: подключение по сети в одной из ролей",
	Long: "Сервер держит роли у себя: логин и пароль проверяет он, и подключившийся\n" +
		"не может их обойти. Команды клиента выполняются на сервере, в роли сессии,\n" +
		"над файлами сервера в каталоге server.dir.\n\n" +
		"Настройки — в config.yaml: ssl (шифрование вкл/выкл), tls.min_version,\n" +
		"tls.cert, tls.key, server.listen, server.dir, server.session_timeout,\n" +
		"server.command_timeout. Сертификат создаёт ochan tls init.\n\n" +
		"Без ssl сервер слушает только эту машину: иначе пароли ролей шли бы по\n" +
		"сети открытым текстом. --allow-plaintext снимает запрет.",
	Args:    cobra.NoArgs,
	Example: "ochan tls init --host orchestrator.example\nochan serve --listen 0.0.0.0:7443",
	RunE: func(cmd *cobra.Command, _ []string) error {
		settings := tlsSettings()
		listen := serveListen
		if listen == "" {
			listen = viper.GetString("server.listen")
		}

		dir, err := serveDirectory()
		if err != nil {
			return err
		}

		// Новый ключ допусков: допуски прошлого запуска больше не действуют.
		if err := mutateRoles(func(store *rbac.Store) error { return store.RotateServerKey() }); err != nil {
			return err
		}

		srv := newRemoteServer(dir, log.New(cmd.ErrOrStderr(), "ochan serve: ", log.LstdFlags))
		return srv.Serve(cmd.Context(), listen, settings, servePlaintext, func(url string) {
			cmd.Printf("Сервер слушает %s\n", url)
			cmd.Printf("Каталог:   %s\n", dir)
			if settings.SSL {
				if fp, err := certFingerprint(settings.Cert); err == nil {
					cmd.Printf("Сертификат: %s\n", fp)
				}
			} else {
				cmd.Println("внимание: ssl выключен, соединение не шифруется")
			}
		})
	},
}

func serveDirectory() (string, error) {
	dir := serveDir
	if dir == "" {
		dir = viper.GetString("server.dir")
	}
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = cwd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return "", fmt.Errorf("server.dir: %s — не каталог", abs)
	}
	return abs, nil
}

// newRemoteServer собирает сервер: роли из хранилища этой машины, права —
// те же, что в командной строке, команды — отдельным процессом ochan.
func newRemoteServer(dir string, logger *log.Logger) *remote.Server {
	prepareCommandTree()
	return &remote.Server{
		LoadRoles: func() (*rbac.Store, error) {
			store, _, err := loadRoles()
			return store, err
		},
		Authorize:      remoteAuthorize,
		Exec:           childExecutor(dir, viper.GetDuration("server.command_timeout")),
		Logger:         logger,
		SessionTimeout: viper.GetDuration("server.session_timeout"),
	}
}

// remoteOnly — команды, которые на сервере не выполняются: оболочка и
// запуск сервера бессмысленны в чужом процессе, а роль сессии переключает
// сам сервер.
var remoteOnly = map[string]string{
	"shell":     "интерактивной оболочки на сервере нет: используйте ochan connect",
	"serve":     "сервер уже запущен",
	"connect":   "подключение к другому серверу с сервера недоступно",
	"remote":    "подключение к другому серверу с сервера недоступно",
	"set.role":  "роль сессии переключает клиент: set role в ochan connect",
	"back.role": "роль сессии переключает клиент: back role в ochan connect",
}

// findMu защищает поиск команды: cobra лениво сортирует подкоманды и
// создаёт наборы флагов, а запросы сервера идут параллельно.
var findMu sync.Mutex

// prepareCommandTree заранее проходит дерево команд, чтобы ленивая
// инициализация cobra случилась до первого запроса.
func prepareCommandTree() {
	findMu.Lock()
	defer findMu.Unlock()
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.Flags()
		c.NonInheritedFlags()
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)
}

// remoteAuthorize решает, может ли роль сессии выполнить команду, — по тем
// же правам, что и в командной строке.
func remoteAuthorize(role *rbac.Role, args []string) error {
	for _, arg := range args {
		// Чужой конфиг мог бы увести токен API на чужой адрес.
		if arg == "--config" || strings.HasPrefix(arg, "--config=") {
			return errors.New("флаг --config на сервере недоступен: сервер работает со своим конфигом")
		}
	}

	findMu.Lock()
	found, _, err := rootCmd.Find(args)
	key := ""
	if err == nil {
		key = commandKey(found)
	}
	findMu.Unlock()

	switch {
	case err != nil:
		return fmt.Errorf("неизвестная команда %q", args[0])
	case key == "":
		return errors.New(remoteOnly["shell"])
	}
	if reason, ok := remoteOnly[key]; ok {
		return errors.New(reason)
	}
	if !role.Allows(key) {
		return fmt.Errorf("роль %s не может выполнять %s: нужно право %s", role.Name,
			strings.ReplaceAll(key, ".", " "), requiredPerm(key))
	}
	return nil
}

// childExecutor выполняет команду отдельным процессом ochan в роли сессии.
//
// Отдельный процесс — потому что команды ochan держат состояние в
// глобальных переменных и между собой не параллелятся. Роль процессу
// передаётся допуском сервера: пароль роли уже проверен при входе.
func childExecutor(dir string, timeout time.Duration) remote.Executor {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return func(ctx context.Context, role string, args []string, stdin string) remote.Result {
		fail := func(err error) remote.Result { return remote.Result{Stderr: err.Error() + "\n", Code: 1} }

		self, err := os.Executable()
		if err != nil {
			return fail(fmt.Errorf("не найден исполняемый файл ochan: %w", err))
		}
		store, _, err := loadRoles()
		if err != nil {
			return fail(err)
		}
		grant, err := store.IssueGrant(role, time.Now().Add(timeout+time.Minute))
		if err != nil {
			return fail(err)
		}

		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		c := exec.CommandContext(ctx, self, args...)
		c.Dir = dir
		c.Env = childEnv(role, grant)
		if stdin != "" {
			c.Stdin = strings.NewReader(stdin)
		}
		stdout, stderr := &cappedBuffer{limit: 16 << 20}, &cappedBuffer{limit: 16 << 20}
		c.Stdout, c.Stderr = stdout, stderr
		if runtime.GOOS != "windows" {
			c.Cancel = func() error { return c.Process.Signal(os.Interrupt) }
		}
		c.WaitDelay = 5 * time.Second

		runErr := c.Run()
		result := remote.Result{Stdout: stdout.String(), Stderr: stderr.String()}
		var exitErr *exec.ExitError
		switch {
		case runErr == nil:
		case errors.As(runErr, &exitErr):
			result.Code = exitErr.ExitCode()
			if result.Code <= 0 {
				result.Code = 1
			}
		default:
			result.Code = 1
			result.Stderr += runErr.Error() + "\n"
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Stderr += fmt.Sprintf("превышено время выполнения на сервере (%s): команда остановлена\n", timeout)
		}
		return result
	}
}

// childEnv — окружение дочернего ochan: роль и допуск сервера поверх
// окружения сервера, без унаследованных роли и допуска.
func childEnv(role, grant string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, EnvRole+"=") || strings.HasPrefix(kv, EnvGrant+"=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, EnvRole+"="+role, EnvGrant+"="+grant)
}

// cappedBuffer — буфер с пределом: вывод сверх предела отбрасывается
// с пометкой, а не съедает память сервера.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room < len(p) {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + fmt.Sprintf("\n… вывод обрезан на %d байтах\n", b.limit)
	}
	return b.buf.String()
}

func certFingerprint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("%s: нет сертификата PEM", path)
	}
	return remote.Fingerprint(block.Bytes), nil
}

// ---------------------------------------------------------------------------
// Сертификат
// ---------------------------------------------------------------------------

var tlsCmd = &cobra.Command{
	Use:   "tls",
	Short: "Сертификат сервера: tls init",
}

var tlsInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Создать самоподписанный сертификат сервера",
	Long: "Создаёт ключ ECDSA P-256 и сертификат в tls.key и tls.cert. Клиентам\n" +
		"этот же сертификат указывается как tls.ca: самоподписанный сертификат\n" +
		"сервера — сам себе удостоверяющий центр. Сверьте отпечаток при передаче.",
	Args:    cobra.NoArgs,
	Example: "ochan tls init --host orchestrator.example --host 10.0.0.5",
	RunE: func(cmd *cobra.Command, _ []string) error {
		settings := tlsSettings()
		if !tlsForce {
			for _, path := range []string{settings.Cert, settings.Key} {
				if _, err := os.Stat(path); err == nil {
					return fmt.Errorf("%s уже есть: перезаписать — --force (клиентам придётся раздать новый сертификат)", path)
				}
			}
		}
		// Имена по умолчанию и указанные — без повторов: в сертификате и в
		// выводе каждое имя один раз.
		var hosts []string
		seen := map[string]bool{}
		for _, host := range append([]string{"localhost", "127.0.0.1", "::1"}, tlsHosts...) {
			if host = strings.TrimSpace(host); host != "" && !seen[strings.ToLower(host)] {
				seen[strings.ToLower(host)] = true
				hosts = append(hosts, host)
			}
		}
		fp, err := remote.GenerateCert(settings.Cert, settings.Key, hosts, time.Duration(tlsDays)*24*time.Hour)
		if err != nil {
			return err
		}
		cmd.Printf("Сертификат: %s\n", settings.Cert)
		cmd.Printf("Ключ:       %s (права 0600)\n", settings.Key)
		cmd.Printf("Имена:      %s\n", strings.Join(hosts, ", "))
		cmd.Printf("Отпечаток:  %s\n", fp)
		cmd.Println("Клиентам: скопировать сертификат и указать его в tls.ca их config.yaml")
		return nil
	},
}

// ---------------------------------------------------------------------------
// Клиент
// ---------------------------------------------------------------------------

var connectCmd = &cobra.Command{
	Use:   "connect <host:port>",
	Short: "Подключиться к общему оркестратору",
	Long: "Открывает сессию на сервере в роли --role (по умолчанию master). Если у роли\n" +
		"есть логин или пароль, их спросят. Внутри сессии: команды ochan выполняются\n" +
		"на сервере, role показывает роль сессии, set role и back role переключают её,\n" +
		"exit завершает сессию.\n\n" +
		"Без вопросов — из " + EnvRemoteLogin + " и " + EnvRemotePassword + ".",
	Args:    cobra.ExactArgs(1),
	Example: "ochan connect orchestrator.example:7443 --role Dan",
	RunE: func(cmd *cobra.Command, args []string) error {
		input := bufio.NewReader(roleInput)
		client, err := openSession(cmd, args[0], input)
		if err != nil {
			return err
		}
		return runSession(cmd, client, args[0], input)
	},
}

var remoteCmd = &cobra.Command{
	Use:   "remote <host:port> <команда>...",
	Short: "Выполнить одну команду на общем оркестраторе",
	Long: "Входит в роль --role, выполняет команду на сервере и выходит. Аргумент «-»\n" +
		"передаёт серверу stdin. Флаги remote пишутся до адреса: всё после него —\n" +
		"команда для сервера.",
	Args: cobra.MinimumNArgs(2),
	Example: "ochan remote orchestrator.example:7443 diff prod.conf test.conf\n" +
		"cat новый.conf | ochan remote --role Dan orchestrator.example:7443 validate -",
	RunE: func(cmd *cobra.Command, args []string) error {
		command := args[1:]
		stdin := ""
		for _, arg := range command {
			if arg == "-" {
				data, err := io.ReadAll(roleInput)
				if err != nil {
					return fmt.Errorf("не удалось прочитать stdin: %w", err)
				}
				stdin = string(data)
				break
			}
		}

		// stdin занят данными — вопросы о логине и пароле читать неоткуда.
		input := bufio.NewReader(roleInput)
		if stdin != "" {
			input = bufio.NewReader(strings.NewReader(""))
		}
		client, err := openSession(cmd, args[0], input)
		if err != nil {
			return err
		}
		defer client.Logout(context.Background())

		if pw, err := localPassword(cmd, command, input); err != nil {
			return err
		} else if pw != "" {
			stdin = pw
		}

		result, err := execRemote(cmd, client, command, stdin)
		if err != nil {
			return err
		}
		if result.Code != 0 {
			return fmt.Errorf("команда на сервере завершилась с кодом %d", result.Code)
		}
		return nil
	},
}

// openSession подключается и входит в роль, спрашивая логин и пароль,
// если сервер их ждёт.
func openSession(cmd *cobra.Command, addr string, input *bufio.Reader) (*remote.Client, error) {
	client, err := remote.Dial(addr, tlsSettings(), remotePlain)
	if err != nil {
		return nil, err
	}
	if client.Plain() {
		cmd.PrintErrln("внимание: ssl выключен, соединение не шифруется")
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	login, password := os.Getenv(EnvRemoteLogin), os.Getenv(EnvRemotePassword)
	state, err := client.Login(ctx, remoteRole, login, password)

	var need *remote.AuthNeeded
	if errors.As(err, &need) && login == "" && password == "" {
		login, password, err = askCredentials(cmd, input, need)
		if err != nil {
			return nil, err
		}
		state, err = client.Login(ctx, remoteRole, login, password)
	}
	if err != nil {
		return nil, err
	}
	cmd.Printf("Подключено к %s, роль %s\n", addr, state.Role)
	return client, nil
}

func askCredentials(cmd *cobra.Command, input *bufio.Reader, need *remote.AuthNeeded) (string, string, error) {
	reader := &credentialReader{cmd: cmd, lines: input}
	var login, password string
	var err error
	if need.Login {
		if login, err = reader.line("Логин: "); err != nil {
			return "", "", err
		}
	}
	if need.Password {
		if password, err = reader.secret("Пароль: "); err != nil {
			return "", "", err
		}
	}
	return login, password, nil
}

// runSession — интерактивная сессия на сервере.
func runSession(cmd *cobra.Command, client *remote.Client, addr string, input *bufio.Reader) error {
	defer client.Logout(context.Background())

	state, err := client.WhoAmI(context.Background())
	if err != nil {
		return err
	}
	role := state.Role

	for {
		cmd.Printf("ochan@%s[%s]> ", addr, role)
		line, err := input.ReadString('\n')
		if err != nil && line == "" {
			cmd.Println()
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}

		args, err := splitArgs(line)
		if err != nil {
			cmd.PrintErrf("ошибка: %v\n", err)
			continue
		}
		if next, err := sessionCommand(cmd, client, input, args); err != nil {
			cmd.PrintErrf("ошибка: %v\n", err)
		} else if next != "" {
			role = next
		}
	}
}

// sessionCommand выполняет строку сессии и возвращает новую роль, если
// она сменилась.
func sessionCommand(cmd *cobra.Command, client *remote.Client, input *bufio.Reader, args []string) (string, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch {
	case len(args) == 1 && args[0] == "role":
		state, err := client.WhoAmI(ctx)
		if err != nil {
			return "", err
		}
		cmd.Printf("Текущая роль: %s\nИстория:      %s\n", state.Role, strings.Join(state.History, " -> "))
		return "", nil

	case len(args) == 3 && args[0] == "set" && args[1] == "role":
		state, err := client.SetRole(ctx, args[2], "", "")
		var need *remote.AuthNeeded
		if errors.As(err, &need) {
			login, password, askErr := askCredentials(cmd, input, need)
			if askErr != nil {
				return "", askErr
			}
			state, err = client.SetRole(ctx, args[2], login, password)
		}
		if err != nil {
			return "", err
		}
		cmd.Printf("Роль: %s\n", state.Role)
		return state.Role, nil

	case len(args) >= 2 && len(args) <= 3 && args[0] == "back" && args[1] == "role":
		target := ""
		if len(args) == 3 {
			target = args[2]
		}
		state, err := client.BackRole(ctx, target)
		if err != nil {
			return "", err
		}
		cmd.Printf("Роль: %s\n", state.Role)
		return state.Role, nil
	}

	for _, arg := range args {
		if arg == "-" {
			return "", errors.New("stdin в интерактивной сессии не передаётся: используйте ochan remote")
		}
	}
	stdin, err := localPassword(cmd, args, input)
	if err != nil {
		return "", err
	}
	_, err = execRemote(cmd, client, args, stdin)
	return "", err
}

// localPassword спрашивает новый пароль для role create/update --npsw
// здесь, у клиента: на сервере спросить некого. Пароль уходит серверу
// на stdin команды.
func localPassword(cmd *cobra.Command, args []string, input *bufio.Reader) (string, error) {
	if len(args) < 3 || args[0] != "role" || (args[1] != "create" && args[1] != "update") {
		return "", nil
	}
	for _, arg := range args {
		if arg == "--npsw" || arg == "--npsw=true" {
			reader := &credentialReader{cmd: cmd, lines: input}
			password, err := reader.newPassword(args[2])
			if err != nil {
				return "", err
			}
			return password + "\n", nil
		}
	}
	return "", nil
}

func execRemote(cmd *cobra.Command, client *remote.Client, args []string, stdin string) (*remote.Result, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	result, err := client.Exec(ctx, args, stdin)
	if err != nil {
		return nil, err
	}
	cmd.Print(result.Stdout)
	if result.Stderr != "" {
		cmd.PrintErr(result.Stderr)
	}
	if result.Code != 0 {
		cmd.PrintErrf("(код %d)\n", result.Code)
	}
	return result, nil
}

func init() {
	serveCmd.Flags().StringVar(&serveListen, "listen", "", "адрес сервера (по умолчанию server.listen)")
	serveCmd.Flags().StringVar(&serveDir, "dir", "", "каталог, над файлами которого работают команды (по умолчанию server.dir или текущий)")
	serveCmd.Flags().BoolVar(&servePlaintext, "allow-plaintext", false, "разрешить работу без ssl на сетевом адресе")

	tlsInitCmd.Flags().StringArrayVar(&tlsHosts, "host", nil, "имя или адрес сервера в сертификате; можно указать несколько раз")
	tlsInitCmd.Flags().IntVar(&tlsDays, "days", 365, "срок действия сертификата в днях")
	tlsInitCmd.Flags().BoolVar(&tlsForce, "force", false, "перезаписать существующие сертификат и ключ")
	tlsCmd.AddCommand(tlsInitCmd)

	for _, c := range []*cobra.Command{connectCmd, remoteCmd} {
		c.Flags().StringVar(&remoteRole, "role", rbac.Master, "роль на сервере")
		c.Flags().BoolVar(&remotePlain, "allow-plaintext", false, "разрешить подключение без ssl к другой машине")
	}
	// Флаги после адреса принадлежат команде на сервере, а не remote.
	remoteCmd.Flags().SetInterspersed(false)
}
