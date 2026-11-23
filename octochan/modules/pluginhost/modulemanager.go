package pluginhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// checksumFilePerm — файл контрольной суммы читаем только владельцем.
const checksumFilePerm os.FileMode = 0o600

// ModuleExtension — расширение файлов плагинов.
const ModuleExtension = ".hcplugin"

// Handshake общий для хоста и плагинов.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "OCTOCHAN_PLUGIN",
	MagicCookieValue: "octochan-2025",
}

// PluginMap — набор плагинов, ожидаемых хостом.
func PluginMap() map[string]plugin.Plugin {
	return map[string]plugin.Plugin{"command": &CommandPlugin{}}
}

// CommandModule — интерфейс, который реализует плагин.
type CommandModule interface {
	GetCommands() []*cobra.Command
	Init(config map[string]any) error
	Name() string
	Version() string
}

// Language — язык, на который модуль переводит ochan: код для lang в
// config.yaml, название и словарь «русский текст → перевод».
type Language struct {
	Code    string
	Name    string
	Entries map[string]string
	// Module — имя модуля, который принёс язык; заполняет хост.
	Module string
}

// Translator реализует модуль языка. Модулю команд реализовывать его не
// нужно: хост спрашивает языки у каждого модуля, и модуль без языка
// отвечает «нет».
type Translator interface {
	Translations() []Language
}

// Setting — настройка модуля: что пользователь задаёт в config.yaml, в
// разделе modules.<имя модуля>. ochan пишет её туда при !!install — с
// пояснением и значением по умолчанию — и при каждой загрузке передаёт
// модулю значения в Init: заданное или Default. Секретов здесь нет:
// токены — ochan auth --executor <имя>.
type Setting struct {
	Key         string // имя в разделе модуля: api_url, timeout
	Default     string // значение по умолчанию текстом (YAML: 30s, [a, b]); пусто — нет
	Description string // что задаёт: пояснение в config.yaml
	Required    bool   // без значения модуль не работает
}

// Configurable реализует модуль с настройками: ochan спрашивает их при
// загрузке. Модулю без настроек реализовывать его не нужно.
type Configurable interface {
	Settings() []Setting
}

// settingKey — имя настройки: оно становится ключом YAML в разделе модуля,
// поэтому без точек, пробелов и двоеточий.
var settingKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// CheckSettings проверяет объявленные настройки: имя — буквы, цифры, «_»
// и «-», первой — буква; имена не повторяются. Иначе модуль не грузится:
// автор узнает об этом при первой же установке.
func CheckSettings(settings []Setting) error {
	seen := map[string]bool{}
	for _, s := range settings {
		if !settingKey.MatchString(s.Key) {
			return fmt.Errorf("настройка %q: имя — буквы, цифры, «_» и «-», первой — буква (оно станет ключом в config.yaml)", s.Key)
		}
		if seen[s.Key] {
			return fmt.Errorf("настройка %s объявлена дважды", s.Key)
		}
		seen[s.Key] = true
	}
	return nil
}

// SettingValues — значения настроек модуля для Init: их даёт ochan (из
// config.yaml). nil — только значения по умолчанию. missing — обязательные
// настройки без значения.
var SettingValues func(module string, declared []Setting) (values map[string]string, missing []string)

// CommandSupply — что ochan передаёт команде модуля в каждом вызове:
// настройки модуля сейчас и его токен. nil — ничего.
var CommandSupply func(module string, declared []Setting) Supply

// supplyKey — ключ Supply в контексте команды модуля.
type supplyKey struct{}

// Token — токен модуля в команде: ochan auth --executor <модуль> или
// OCTOCHAN_TOKEN_<МОДУЛЬ>; пусто — не задан или ochan старше 0.3.1. ctx —
// cmd.Context() команды.
func Token(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	supply, _ := ctx.Value(supplyKey{}).(Supply)
	return supply.Credentials["token"]
}

// Settings — настройки модуля в команде, как сейчас в config.yaml
// (modules.<модуль>), иначе умолчания модуля: правка конфига видна без
// перезагрузки модуля. Пусто — ochan старше 0.3.1: берите то, что пришло
// в Init. ctx — cmd.Context() команды.
func Settings(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	supply, _ := ctx.Value(supplyKey{}).(Supply)
	return supply.Settings
}

// CommandPlugin связывает CommandModule с go-plugin.
type CommandPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl CommandModule
}

func (p *CommandPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	RegisterCommandModuleServer(s, &CommandGRPCServer{Impl: p.Impl})
	return nil
}

func (p *CommandPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &CommandGRPCClient{Client: NewCommandModuleClient(c)}, nil
}

// EnvPluginLogLevel задаёт уровень журналирования внутри процесса плагина.
const EnvPluginLogLevel = "OCTOCHAN_PLUGIN_LOG_LEVEL"

// ServeCommandModule запускает плагин. Вся обвязка go-plugin/gRPC живёт здесь,
// поэтому бинарнику плагина достаточно реализовать CommandModule.
func ServeCommandModule(impl CommandModule) {
	// По умолчанию Warn: на Info go-plugin печатает служебные строки вроде
	// "configuring server automatic mTLS" при каждом вызове любой команды.
	level := hclog.Warn
	if configured := os.Getenv(EnvPluginLogLevel); configured != "" {
		level = hclog.LevelFromString(configured)
	}

	logger := hclog.New(&hclog.LoggerOptions{
		Name:   impl.Name(),
		Output: os.Stderr,
		Level:  level,
	})

	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         map[string]plugin.Plugin{"command": &CommandPlugin{Impl: impl}},
		GRPCServer:      plugin.DefaultGRPCServer,
		Logger:          logger,
	})
}

// ---------------------------------------------------------------------------
// Серверная сторона (внутри процесса плагина)
// ---------------------------------------------------------------------------

// CommandGRPCServer обслуживает вызовы хоста внутри плагина.
type CommandGRPCServer struct {
	UnimplementedCommandModuleServer
	Impl CommandModule
}

func (s *CommandGRPCServer) GetCommands(_ context.Context, _ *Empty) (*CommandsResponse, error) {
	return &CommandsResponse{Commands: CommandsToProto(s.Impl.GetCommands())}, nil
}

func (s *CommandGRPCServer) Name(_ context.Context, _ *Empty) (*NameResponse, error) {
	return &NameResponse{Name: s.Impl.Name()}, nil
}

func (s *CommandGRPCServer) Version(_ context.Context, _ *Empty) (*VersionResponse, error) {
	return &VersionResponse{Version: s.Impl.Version()}, nil
}

func (s *CommandGRPCServer) GetTranslations(_ context.Context, _ *Empty) (*TranslationsResponse, error) {
	resp := &TranslationsResponse{}
	translator, ok := s.Impl.(Translator)
	if !ok {
		return resp, nil
	}
	for _, lang := range translator.Translations() {
		resp.Languages = append(resp.Languages, &Translation{Code: lang.Code, Name: lang.Name, Entries: lang.Entries})
	}
	return resp, nil
}

func (s *CommandGRPCServer) GetSettings(_ context.Context, _ *Empty) (*SettingsResponse, error) {
	resp := &SettingsResponse{}
	configurable, ok := s.Impl.(Configurable)
	if !ok {
		return resp, nil
	}
	for _, setting := range configurable.Settings() {
		resp.Settings = append(resp.Settings, &SettingSpec{Key: setting.Key, Default: setting.Default,
			Description: setting.Description, Required: setting.Required})
	}
	return resp, nil
}

func (s *CommandGRPCServer) Init(_ context.Context, cfg *Config) (*Empty, error) {
	values := make(map[string]any, len(cfg.GetValues()))
	for k, v := range cfg.GetValues() {
		values[k] = v
	}
	if err := s.Impl.Init(values); err != nil {
		return nil, err
	}
	return &Empty{}, nil
}

// resolveCommand находит команду по пути вида "test generate".
func (s *CommandGRPCServer) resolveCommand(path string) (*cobra.Command, error) {
	segments := strings.Fields(path)
	if len(segments) == 0 {
		return nil, fmt.Errorf("не указано имя команды")
	}

	var current *cobra.Command
	candidates := s.Impl.GetCommands()

	for _, segment := range segments {
		var found *cobra.Command
		for _, candidate := range candidates {
			if commandName(candidate.Use) == segment || candidate.Name() == segment {
				found = candidate
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("команда %q не найдена в модуле", path)
		}
		current = found
		candidates = found.Commands()
	}

	return current, nil
}

// outputTrailer — вывод команды модуля в trailer-метаданных ответа gRPC.
// Так вывод доходит до хоста вместе с ответом и попадает туда, куда хост
// направил вывод команды: в ochan() сценария, в конвейер, удалённому
// клиенту. Раньше он шёл в stdout процесса модуля, а оттуда — прямо в
// stdout ochan, мимо всего этого. Ключ с -bin — байты как есть.
const outputTrailer = "ochan-output-bin"

// maxTrailerOutput — больше этого вывод идёт по-старому, в stdout модуля:
// метаданные gRPC — не для мегабайтов.
const maxTrailerOutput = 4 << 20

// commandMu — команды модуля выполняются по одной: stdout процесса на время
// команды — её, и двух сразу он не различит.
var commandMu sync.Mutex

// lockedBuffer — вывод команды из двух мест сразу: cmd и stdout процесса.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Bytes()
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureStdout направляет stdout процесса модуля в w до вызова stop: stop
// дожидается всего, что успели напечатать, и возвращает stdout на место.
// Не вышло завести трубу — stdout остаётся как был.
func captureStdout(w io.Writer) (stop func()) {
	r, pw, err := os.Pipe()
	if err != nil {
		return func() {}
	}
	saved := os.Stdout
	os.Stdout = pw
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(w, r)
		close(done)
	}()
	return func() {
		os.Stdout = saved
		_ = pw.Close()
		<-done
		_ = r.Close()
	}
}

// runCommand — общая реализация для Run/PreRun/PostRun. Вывод команды
// (cmd.Println, cmd.OutOrStdout()) возвращается хосту в trailer.
func (s *CommandGRPCServer) runCommand(ctx context.Context, req *CommandRequest, phase string) (*Empty, error) {
	cmd, err := s.resolveCommand(req.GetName())
	if err != nil {
		return nil, err
	}
	// Вывод команды — и cmd.Println, и fmt.Println мимо cmd (stdout
	// процесса модуля): всё уходит хосту вместе с ответом. Раньше stdout
	// процесса go-plugin пересылал отдельно и позже ответа, и строка
	// попадала туда, куда хост смотрел в момент пересылки: «строка >| файл»
	// получала 0 байт, а вывод — следующий файл или экран.
	commandMu.Lock()
	defer commandMu.Unlock()
	// Настройки и токен модуля — в контексте команды: pluginhost.Settings
	// и pluginhost.Token.
	prevCtx := cmd.Context()
	cmd.SetContext(context.WithValue(ctx, supplyKey{}, Supply{Credentials: req.GetCredentials(),
		Settings: req.GetSettings()}))
	defer cmd.SetContext(prevCtx)
	var output lockedBuffer
	cmd.SetOut(&output)
	stopCapture := captureStdout(&output)
	defer func() {
		stopCapture()
		cmd.SetOut(nil)
		if output.Len() == 0 {
			return
		}
		if output.Len() > maxTrailerOutput {
			_, _ = os.Stdout.Write(output.Bytes())
			return
		}
		_ = grpc.SetTrailer(ctx, metadata.Pairs(outputTrailer, output.String()))
	}()

	for name, value := range req.GetFlags() {
		if err := cmd.Flags().Set(name, value); err != nil {
			return nil, fmt.Errorf("не удалось установить флаг %s: %w", name, err)
		}
	}

	args := req.GetArgs()

	switch phase {
	case "pre":
		if cmd.PreRunE != nil {
			return &Empty{}, cmd.PreRunE(cmd, args)
		}
		if cmd.PreRun != nil {
			cmd.PreRun(cmd, args)
		}
	case "post":
		if cmd.PostRunE != nil {
			return &Empty{}, cmd.PostRunE(cmd, args)
		}
		if cmd.PostRun != nil {
			cmd.PostRun(cmd, args)
		}
	default:
		// Аргументы валидируются так же, как это сделала бы cobra локально.
		if cmd.Args != nil {
			if err := cmd.Args(cmd, args); err != nil {
				return nil, err
			}
		}
		if cmd.RunE != nil {
			return &Empty{}, cmd.RunE(cmd, args)
		}
		if cmd.Run != nil {
			cmd.Run(cmd, args)
		}
	}

	return &Empty{}, nil
}

func (s *CommandGRPCServer) RunCommand(ctx context.Context, req *CommandRequest) (*Empty, error) {
	return s.runCommand(ctx, req, "run")
}

func (s *CommandGRPCServer) PreRunCommand(ctx context.Context, req *CommandRequest) (*Empty, error) {
	return s.runCommand(ctx, req, "pre")
}

func (s *CommandGRPCServer) PostRunCommand(ctx context.Context, req *CommandRequest) (*Empty, error) {
	return s.runCommand(ctx, req, "post")
}

// ---------------------------------------------------------------------------
// Клиентская сторона (внутри octochan)
// ---------------------------------------------------------------------------

// CommandGRPCClient — представление модуля на стороне хоста.
type CommandGRPCClient struct {
	Client CommandModuleClient
	// module и settings — имя модуля и его настройки: по ним команде
	// выдаются значения и токен (CommandSupply).
	module   string
	settings []Setting
}

// commandTimeout — предельное время выполнения команды модуля.
// Прежние 10 секунд не переживал ни один рестарт СУБД.
func commandTimeout() time.Duration {
	if d := viper.GetDuration("defaults.module_command_timeout"); d > 0 {
		return d
	}
	return 10 * time.Minute
}

func (c *CommandGRPCClient) GetCommands() []*cobra.Command {
	resp, err := c.Client.GetCommands(context.Background(), &Empty{})
	if err != nil {
		log.Printf("не удалось получить команды модуля: %v", err)
		return nil
	}
	return c.CreateCommandsFromProto(resp.GetCommands(), nil)
}

// CreateCommandsFromProto строит команды cobra по описанию из плагина.
// parentPath хранит путь до родителя, чтобы вызов подкоманды адресовался
// как "test generate", а не по строке Use целиком.
func (c *CommandGRPCClient) CreateCommandsFromProto(pbCommands []*Command, parentPath []string) []*cobra.Command {
	commands := make([]*cobra.Command, 0, len(pbCommands))

	for _, pbCmd := range pbCommands {
		if pbCmd == nil {
			continue
		}

		name := commandName(pbCmd.GetUse())
		if name == "" {
			continue
		}
		path := append(append([]string{}, parentPath...), name)
		remotePath := strings.Join(path, " ")

		cmd := &cobra.Command{
			Use:                    pbCmd.GetUse(),
			Aliases:                pbCmd.GetAliases(),
			Short:                  pbCmd.GetShort(),
			Long:                   pbCmd.GetLong(),
			Example:                pbCmd.GetExample(),
			ValidArgs:              pbCmd.GetValidArgs(),
			ArgAliases:             pbCmd.GetArgAliases(),
			BashCompletionFunction: pbCmd.GetBashCompletionFunction(),
			Deprecated:             pbCmd.GetDeprecated(),
			Hidden:                 pbCmd.GetHidden(),
			Annotations:            pbCmd.GetAnnotations(),
			Version:                pbCmd.GetVersion(),
			SilenceErrors:          pbCmd.GetSilenceErrors(),
			SilenceUsage:           pbCmd.GetSilenceUsage(),
			TraverseChildren:       pbCmd.GetTraverseChildren(),
			FParseErrWhitelist: cobra.FParseErrWhitelist{
				UnknownFlags: pbCmd.GetFparseErrWhitelist().GetUnknownFlags(),
			},
		}

		applyFlags(cmd.Flags(), pbCmd.GetFlags())
		applyFlags(cmd.PersistentFlags(), pbCmd.GetPersistentFlags())

		if pbCmd.GetRunFunction() {
			cmd.RunE = c.remoteRunner(remotePath, c.Client.RunCommand)
		}
		if pbCmd.GetPreRunFunction() {
			cmd.PreRunE = c.remoteRunner(remotePath, c.Client.PreRunCommand)
		}
		if pbCmd.GetPostRunFunction() {
			cmd.PostRunE = c.remoteRunner(remotePath, c.Client.PostRunCommand)
		}

		for _, sub := range c.CreateCommandsFromProto(pbCmd.GetCommands(), path) {
			cmd.AddCommand(sub)
		}

		commands = append(commands, cmd)
	}

	return commands
}

type remoteCall func(context.Context, *CommandRequest, ...grpc.CallOption) (*Empty, error)

// remoteRunner возвращает обработчик, пробрасывающий вызов в плагин.
// Ошибка возвращается наружу (RunE), а не печатается — иначе octochan
// завершался с кодом 0 даже при падении команды модуля.
func (c *CommandGRPCClient) remoteRunner(path string, call remoteCall) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), commandTimeout())
		defer cancel()

		// Передаём только явно заданные флаги: значения по умолчанию
		// плагин знает и сам. Раньше флаги не передавались вовсе, и команда
		// модуля всегда выполнялась с дефолтами.
		flags := make(map[string]string)
		cmd.Flags().Visit(func(f *pflag.Flag) {
			flags[f.Name] = f.Value.String()
		})
		cmd.PersistentFlags().Visit(func(f *pflag.Flag) {
			flags[f.Name] = f.Value.String()
		})

		// Вывод команды приходит в trailer (outputTrailer); модуль, собранный
		// до этого, печатает в свой stdout, и trailer пуст.
		req := &CommandRequest{Name: path, Args: args, Flags: flags}
		if CommandSupply != nil && c.module != "" {
			supply := CommandSupply(c.module, c.settings)
			req.Settings, req.Credentials = supply.Settings, supply.Credentials
		}
		var trailer metadata.MD
		_, err := call(ctx, req, grpc.Trailer(&trailer))
		for _, chunk := range trailer.Get(outputTrailer) {
			_, _ = io.WriteString(cmd.OutOrStdout(), chunk)
		}
		return unwrapGRPCError(err)
	}
}

// unwrapGRPCError убирает обёртку транспорта из сообщения об ошибке:
// пользователю нужен текст, который вернул модуль, а не
// "rpc error: code = Unknown desc = ...".
func unwrapGRPCError(err error) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if !ok {
		return err
	}

	switch st.Code() {
	case codes.DeadlineExceeded:
		return fmt.Errorf("модуль не ответил за %s: %s", commandTimeout(), st.Message())
	case codes.Unavailable:
		return fmt.Errorf("модуль недоступен: %s", st.Message())
	default:
		return errors.New(st.Message())
	}
}

func (c *CommandGRPCClient) Init(config map[string]any) error {
	values := make(map[string]string, len(config))
	for k, v := range config {
		values[k] = fmt.Sprintf("%v", v)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := c.Client.Init(ctx, &Config{Values: values})
	return err
}

func (c *CommandGRPCClient) Name() string {
	name, _ := c.ProbeName()
	return name
}

// ProbeName возвращает имя модуля вместе с причиной отказа.
//
// Интерфейс CommandModule требует Name() string без ошибки, и раньше причина
// молча терялась: при несовпадении версии протокола пользователь видел
// бессмысленное «модуль не сообщил имя» вместо объяснения, что модуль
// собран под другую версию и его надо пересобрать.
func (c *CommandGRPCClient) ProbeName() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Client.Name(ctx, &Empty{})
	if err != nil {
		return "", protocolError(err)
	}
	return resp.GetName(), nil
}

// protocolError распознаёт модуль, собранный под другую версию описания
// сервиса: gRPC отвечает Unimplemented, если имя сервиса не совпало.
func protocolError(err error) error {
	if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
		return fmt.Errorf("модуль собран под другую версию протокола и требует пересборки: %s", st.Message())
	}
	return unwrapGRPCError(err)
}

// Translations спрашивает у модуля его языки. Модуль, собранный до
// появления языков, отвечает Unimplemented: языков у него нет, и это не
// ошибка.
func (c *CommandGRPCClient) Translations() ([]Language, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Client.GetTranslations(ctx, &Empty{})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
			return nil, nil
		}
		return nil, unwrapGRPCError(err)
	}
	var out []Language
	for _, t := range resp.GetLanguages() {
		out = append(out, Language{Code: t.GetCode(), Name: t.GetName(), Entries: t.GetEntries()})
	}
	return out, nil
}

// Settings — настройки модуля; модуль старее GetSettings — их нет.
func (c *CommandGRPCClient) Settings() ([]Setting, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Client.GetSettings(ctx, &Empty{})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unimplemented {
			return nil, nil
		}
		return nil, unwrapGRPCError(err)
	}
	var out []Setting
	for _, s := range resp.GetSettings() {
		out = append(out, Setting{Key: s.GetKey(), Default: s.GetDefault(), Description: s.GetDescription(),
			Required: s.GetRequired()})
	}
	return out, nil
}

func (c *CommandGRPCClient) Version() string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Client.Version(ctx, &Empty{})
	if err != nil {
		return ""
	}
	return resp.GetVersion()
}

// ---------------------------------------------------------------------------
// Менеджер модулей
// ---------------------------------------------------------------------------

type loadedModule struct {
	name      string
	version   string
	path      string
	client    *plugin.Client
	commands  []*cobra.Command
	languages []Language
	runners   []RemoteRunner
	settings  []Setting
	missing   []string
}

// ModuleManager владеет процессами плагинов и их командами.
type ModuleManager struct {
	mu      sync.Mutex
	modules map[string]*loadedModule // ключ — путь до файла модуля
}

var globalModuleManager = NewModuleManager()

// NewModuleManager создаёт независимый менеджер модулей.
// Нужен, например, команде inspect: она поднимает модуль во временном
// менеджере и гасит его, не трогая набор команд текущей сессии.
func NewModuleManager() *ModuleManager {
	return &ModuleManager{modules: make(map[string]*loadedModule)}
}

// GetModuleManager возвращает менеджер модулей процесса.
func GetModuleManager() *ModuleManager { return globalModuleManager }

// ErrChecksumMissing возвращается, если рядом с модулем нет файла контрольной суммы.
var ErrChecksumMissing = errors.New("отсутствует контрольная сумма модуля")

// ChecksumPath — путь до файла с контрольной суммой модуля.
func ChecksumPath(modulePath string) string { return modulePath + ".sha256" }

// FileChecksum считает SHA-256 файла.
func FileChecksum(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// WriteChecksum фиксирует текущее содержимое модуля как доверенное.
func WriteChecksum(modulePath string) error {
	sum, err := FileChecksum(modulePath)
	if err != nil {
		return fmt.Errorf("не удалось посчитать контрольную сумму: %w", err)
	}
	return os.WriteFile(ChecksumPath(modulePath), []byte(hex.EncodeToString(sum)), checksumFilePerm)
}

func expectedChecksum(modulePath string) ([]byte, error) {
	data, err := os.ReadFile(ChecksumPath(modulePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrChecksumMissing
		}
		return nil, err
	}

	sum, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("повреждён файл контрольной суммы %s: %w", ChecksumPath(modulePath), err)
	}
	return sum, nil
}

// currentStdout пишет в тот os.Stdout, что сейчас: оболочка направляет
// вывод строки в файл (>|), подменяя os.Stdout, а модуль загружен раньше.
type currentStdout struct{}

func (currentStdout) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

// LoadModule запускает плагин и регистрирует его команды.
//
// Плагин — это исполняемый файл, который запускается отдельным процессом.
// Раньше загружалось всё, что появилось в директории модулей, без проверок,
// а go-plugin настраивался без SecureConfig и AutoMTLS.
func (m *ModuleManager) LoadModule(path string) error {
	return m.load(path, nil)
}

// Probe запускает модуль из файла, который ещё не установлен, — как
// установленный, с суммой самого файла, — и гасит его: так !!install узнаёт,
// модуль ли это, до того как что-то скопировано, а прежняя версия цела.
// Возвращает то, что модуль объявил.
func Probe(path string) (ModuleInfo, error) {
	probe, err := OpenProbe(path)
	if err != nil {
		return ModuleInfo{}, err
	}
	defer probe.Cleanup()
	infos := probe.LoadedModules()
	if len(infos) == 0 {
		return ModuleInfo{}, errors.New("модуль не сообщил о себе ничего")
	}
	return infos[0], nil
}

// OpenProbe запускает модуль из файла в отдельном менеджере — с суммой
// самого файла, без установки — и отдаёт менеджер: команды, исполнители и
// настройки модуля. Гасит его Cleanup вызывающего.
func OpenProbe(path string) (*ModuleManager, error) {
	sum, err := FileChecksum(path)
	if err != nil {
		return nil, err
	}
	probe := NewModuleManager()
	if err := probe.load(path, &plugin.SecureConfig{Checksum: sum, Hash: sha256.New()}); err != nil {
		probe.Cleanup()
		return nil, err
	}
	return probe, nil
}

// ErrNotModule — файл не модуль ochan: его не запустить или он не отвечает
// рукопожатием модуля.
var ErrNotModule = errors.New("не модуль ochan")

// buildHint — как собрать модуль.
const buildHint = "соберите модуль: package main и main() { pluginhost.ServeCommandModule(…) }"

// CheckModuleFile смотрит на файл до запуска: программа ли это вообще.
// go build пакета не main пишет не программу, а архив пакета (!<arch>), и
// запуск его кончался голым «exec format error».
func CheckModuleFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 8)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, []byte("!<arch>\n")):
		return fmt.Errorf("%w: %s — архив пакета Go, а не программа: собран пакет не main (go build пакета-библиотеки "+
			"даёт архив); %s", ErrNotModule, filepath.Base(path), buildHint)
	case bytes.HasPrefix(head, []byte("\x7fELF")), bytes.HasPrefix(head, []byte("MZ")), bytes.HasPrefix(head, []byte("#!")),
		len(head) >= 4 && (bytes.Equal(head[:4], []byte{0xcf, 0xfa, 0xed, 0xfe}) || bytes.Equal(head[:4], []byte{0xce, 0xfa, 0xed, 0xfe}) ||
			bytes.Equal(head[:4], []byte{0xca, 0xfe, 0xba, 0xbe})):
		return nil
	}
	return fmt.Errorf("%w: %s — не программа (не ELF, Mach-O, PE и не сценарий с #!); %s", ErrNotModule, filepath.Base(path), buildHint)
}

// stderrTail — последние строки stderr процесса модуля: их показывает
// ошибка загрузки — паника или сообщение модуля, а не голый «exit status 2».
type stderrTail struct {
	mu   sync.Mutex
	data []byte
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if over := len(t.data) - 16<<10; over > 0 {
		t.data = t.data[over:]
	}
	return len(p), nil
}

// lines — последние n строк.
func (t *stderrTail) lines(n int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	all := strings.Split(strings.TrimRight(string(t.data), "\n"), "\n")
	if len(all) == 1 && all[0] == "" {
		return nil
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// withStderr дописывает к ошибке запуска хвост stderr модуля.
func (t *stderrTail) withStderr(err error) error {
	lines := t.lines(20)
	if len(lines) == 0 {
		return err
	}
	return fmt.Errorf("%w\n  stderr модуля:\n    %s", err, strings.Join(lines, "\n    "))
}

func (m *ModuleManager) load(path string, secure *plugin.SecureConfig) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("не удалось определить путь модуля: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("модуль недоступен: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s — это директория, а не модуль", absPath)
	}

	if err := CheckModuleFile(absPath); err != nil {
		return err
	}
	secureConfig := secure
	if secureConfig == nil {
		if secureConfig, err = m.secureConfigFor(absPath); err != nil {
			return err
		}
	}
	stderr := &stderrTail{}

	logger := hclog.New(&hclog.LoggerOptions{
		Name: "plugin-loader",
		// Логи плагина идут в stderr: stdout занят полезным выводом команд
		// и раньше отладочные строки ломали конвейеры.
		Output: os.Stderr,
		Level:  pluginLogLevel(),
	})

	moduleCmd := exec.Command(absPath)
	moduleCmd.Env = append(os.Environ(),
		EnvPluginLogLevel+"="+viper.GetString("defaults.plugin_log_level"))

	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          PluginMap(),
		Cmd:              moduleCmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger:           logger,
		StartTimeout:     30 * time.Second,
		SecureConfig:     secureConfig,
		AutoMTLS:         true,
		// Без этого вывод команд модуля пропадал: go-plugin перехватывает
		// stdout дочернего процесса и по умолчанию отдаёт его в свой логгер
		// на уровне debug, то есть в никуда.
		SyncStdout: currentStdout{},
		SyncStderr: os.Stderr,
		// Сырой stderr процесса — и до рукопожатия: паника при запуске видна
		// в ошибке загрузки.
		Stderr: stderr,
	})

	// Процесс гасится на любом пути выхода, кроме успешного: до этого
	// Kill стоял в каждой ветке отдельно, и достаточно было добавить новую
	// проверку, чтобы процесс модуля остался висеть и держать свой файл.
	adopted := false
	defer func() {
		if !adopted {
			client.Kill()
		}
	}()

	module, err := dispenseCommandModule(client)
	if err != nil {
		// go-plugin объясняет отказ рукопожатия на десяток строк про
		// архитектуру и права; суть — одна строка, а причина — в stderr.
		reason := strings.SplitN(err.Error(), "\n", 2)[0]
		switch {
		case client.Exited():
			err = fmt.Errorf("%w: %s завершился, не ответив рукопожатием модуля (%s); %s", ErrNotModule,
				filepath.Base(absPath), strings.TrimSuffix(strings.TrimSpace(reason), ":"), buildHint)
		case strings.Contains(reason, "Unrecognized remote plugin") || strings.Contains(reason, "Incompatible"):
			err = fmt.Errorf("%w: %s не ответил рукопожатием модуля (%s); %s", ErrNotModule, filepath.Base(absPath),
				strings.TrimSuffix(strings.TrimSpace(reason), ":"), buildHint)
		}
		return stderr.withStderr(err)
	}

	name, err := moduleName(module)
	if err != nil {
		return fmt.Errorf("модуль %s: %w", filepath.Base(absPath), err)
	}
	if name == "" {
		return fmt.Errorf("модуль %s не сообщил имя", absPath)
	}

	commands := module.GetCommands()

	// Языки спрашиваются при загрузке, как и команды: модуль языка — такой
	// же модуль, и после !!install он работает со следующего запуска.
	var languages []Language
	if translator, ok := module.(interface{ Translations() ([]Language, error) }); ok {
		languages, err = translator.Translations()
		if err != nil {
			return fmt.Errorf("модуль %s: языки: %w", name, err)
		}
		for i := range languages {
			languages[i].Module = name
		}
	}
	var runners []RemoteRunner
	if provider, ok := module.(interface {
		Runners() ([]RemoteRunner, error)
	}); ok {
		runners, err = provider.Runners()
		if err != nil {
			return fmt.Errorf("модуль %s: исполнители: %w", name, err)
		}
		for i := range runners {
			runners[i].Module = name
		}
	}
	// Настройки — до того, как модуль начнёт работать: он получает их
	// значения в Init. Раньше Init не вызывался вовсе, и настроить модуль
	// было нечем.
	var settings []Setting
	if configurable, ok := module.(interface{ Settings() ([]Setting, error) }); ok {
		settings, err = configurable.Settings()
		if err != nil {
			return fmt.Errorf("модуль %s: настройки: %w", name, err)
		}
		if err := CheckSettings(settings); err != nil {
			return fmt.Errorf("модуль %s: %w", name, err)
		}
	}
	values := map[string]string{}
	var missing []string
	for _, s := range settings {
		if s.Default != "" {
			values[s.Key] = s.Default
		}
	}
	if SettingValues != nil {
		values, missing = SettingValues(name, settings)
	}
	init := make(map[string]any, len(values))
	for k, v := range values {
		init[k] = v
	}
	if err := module.Init(init); err != nil {
		return fmt.Errorf("модуль %s не принял настройки: %w", name, err)
	}
	if client, ok := module.(*CommandGRPCClient); ok {
		client.module, client.settings = name, settings
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Перезагрузка того же файла: гасим прежний процесс, иначе он оставался
	// висеть, а его команды дублировались в списке.
	if prev, ok := m.modules[absPath]; ok {
		prev.client.Kill()
		delete(m.modules, absPath)
	}
	for otherPath, other := range m.modules {
		if other.name == name {
			other.client.Kill()
			delete(m.modules, otherPath)
		}
	}

	m.modules[absPath] = &loadedModule{
		name:      name,
		version:   module.Version(),
		path:      absPath,
		client:    client,
		commands:  commands,
		languages: languages,
		runners:   runners,
		settings:  settings,
		missing:   missing,
	}
	// Владение процессом перешло менеджеру: гасить его будет Cleanup.
	adopted = true

	return nil
}

func (m *ModuleManager) secureConfigFor(absPath string) (*plugin.SecureConfig, error) {
	sum, err := expectedChecksum(absPath)
	switch {
	case err == nil:
		return &plugin.SecureConfig{Checksum: sum, Hash: sha256.New()}, nil
	case errors.Is(err, ErrChecksumMissing):
		if viper.GetBool("defaults.allow_unverified_modules") {
			log.Printf("модуль %s загружается без проверки контрольной суммы "+
				"(defaults.allow_unverified_modules=true)", filepath.Base(absPath))
			return nil, nil
		}
		return nil, fmt.Errorf(
			"%w: выполните '!!install %s', чтобы зафиксировать его как доверенный, "+
				"или включите defaults.allow_unverified_modules",
			ErrChecksumMissing, absPath)
	default:
		return nil, err
	}
}

// moduleName спрашивает имя модуля, сохраняя причину отказа.
//
// Интерфейс CommandModule возвращает имя без ошибки, поэтому причина
// доступна только через необязательный ProbeName.
func moduleName(module CommandModule) (string, error) {
	if prober, ok := module.(interface{ ProbeName() (string, error) }); ok {
		return prober.ProbeName()
	}
	return module.Name(), nil
}

func dispenseCommandModule(client *plugin.Client) (CommandModule, error) {
	rpcClient, err := client.Client()
	if err != nil {
		return nil, fmt.Errorf("не удалось подключиться к модулю: %w", err)
	}

	raw, err := rpcClient.Dispense("command")
	if err != nil {
		return nil, fmt.Errorf("модуль не предоставляет интерфейс command: %w", err)
	}

	module, ok := raw.(CommandModule)
	if !ok {
		return nil, fmt.Errorf("неверный тип модуля: ожидался CommandModule, получен %T", raw)
	}
	return module, nil
}

func pluginLogLevel() hclog.Level {
	if level := viper.GetString("defaults.plugin_log_level"); level != "" {
		return hclog.LevelFromString(level)
	}
	return hclog.Warn
}

// UnloadModule гасит модуль по пути файла.
func (m *ModuleManager) UnloadModule(path string) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if module, ok := m.modules[absPath]; ok {
		module.client.Kill()
		delete(m.modules, absPath)
	}
}

// pathsLocked — пути загруженных модулей по порядку: порядок команд,
// языков и исполнителей не меняется между запусками.
func (m *ModuleManager) pathsLocked() []string {
	paths := make([]string, 0, len(m.modules))
	for path := range m.modules {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// GetCommands возвращает копию списка команд всех модулей.
func (m *ModuleManager) GetCommands() []*cobra.Command {
	m.mu.Lock()
	defer m.mu.Unlock()

	var commands []*cobra.Command
	for _, path := range m.pathsLocked() {
		commands = append(commands, m.modules[path].commands...)
	}
	return commands
}

// Languages возвращает языки всех модулей в порядке путей модулей.
func (m *ModuleManager) Languages() []Language {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Language
	for _, path := range m.pathsLocked() {
		out = append(out, m.modules[path].languages...)
	}
	return out
}

// ModuleInfo — краткие сведения о загруженном модуле.
type ModuleInfo struct {
	Name      string
	Version   string
	Path      string
	Commands  int
	Languages []string
	// Runners — виды исполнителей модуля.
	Runners []string
	// Settings — настройки модуля (modules.<имя> в config.yaml); Missing —
	// обязательные без значения.
	Settings []Setting
	Missing  []string
}

// LoadedModules перечисляет загруженные модули.
func (m *ModuleManager) LoadedModules() []ModuleInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	infos := make([]ModuleInfo, 0, len(m.modules))
	for _, module := range m.modules {
		info := ModuleInfo{
			Name:     module.name,
			Version:  module.version,
			Path:     module.path,
			Commands: len(module.commands),
		}
		for _, lang := range module.languages {
			info.Languages = append(info.Languages, lang.Code)
		}
		for _, r := range module.runners {
			info.Runners = append(info.Runners, r.Kind)
		}
		info.Settings = append([]Setting(nil), module.settings...)
		info.Missing = append([]string(nil), module.missing...)
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos
}

// Cleanup гасит все процессы плагинов.
func (m *ModuleManager) Cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for path, module := range m.modules {
		module.client.Kill()
		delete(m.modules, path)
	}
}

// LoadModulesFromDir загружает все плагины из директории.
// Ошибки отдельных модулей собираются, а не теряются в логе.
func (m *ModuleManager) LoadModulesFromDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("не удалось прочитать директорию модулей: %w", err)
	}

	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ModuleExtension {
			continue
		}
		// Имя модуля LoadModule подставляет само; повторять его здесь
		// значило бы печатать «модуль X: модуль X: ...».
		if err := m.LoadModule(filepath.Join(dir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
