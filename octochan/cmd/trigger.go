package cmd

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"octochan/audit"
	"octochan/i18n"
	"octochan/inventory"
	"octochan/rbac"
	"octochan/remote"
	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Триггеры: роль по событию внешней системы.
//
//	triggers:
//	  release:
//	    event: ci/release          # что шлёт CI; по умолчанию — имя триггера
//	    grant:
//	      role: deployer
//	      scope: [group=prod]
//	      max_window: 2h           # релиз завис — роль гаснет сама
//
// Роль, названная в триггере, вне его окна не может ничего, кроме того,
// что доступно любой роли: узнать, кто ты, и вернуться. Окно открывает
// событие — ochan trigger fire или вебхук POST /v1/events/<триггер> на
// сервере, — закрывает событие конца или max_window. В окне цели роли —
// только под scope.

// triggerConf — триггер из config.yaml.
type triggerConf struct {
	Name      string
	Event     string
	Role      string
	Scope     []string
	MaxWindow time.Duration
	Window    string
}

var triggerName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// triggerConfs читает триггеры из настроек. Ошибка в одном — ошибка всех:
// роль, которую триггер должен держать закрытой, иначе осталась бы открытой.
func triggerConfs() ([]triggerConf, error) {
	raw := viper.GetStringMap("triggers")
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []triggerConf
	for _, name := range names {
		key := "triggers." + name
		t := triggerConf{Name: name, Event: viper.GetString(key + ".event"),
			Role: viper.GetString(key + ".grant.role"), Scope: viper.GetStringSlice(key + ".grant.scope"),
			Window: viper.GetString(key + ".grant.max_window")}
		if !triggerName.MatchString(name) {
			return nil, fmt.Errorf("триггер %q: имя — латинские строчные буквы, цифры, _ и -", name)
		}
		if t.Event == "" {
			t.Event = name
		}
		switch {
		case t.Role == "":
			return nil, fmt.Errorf("триггер %s: grant.role — какую роль открывает событие", name)
		case strings.EqualFold(t.Role, rbac.Master) || strings.EqualFold(t.Role, rbac.LLM):
			return nil, fmt.Errorf("триггер %s: роль %s триггером не открывается — у неё свои правила", name, t.Role)
		case t.Window == "":
			return nil, fmt.Errorf("триггер %s: grant.max_window обязателен — иначе зависший релиз держал бы роль вечно", name)
		}
		d, err := rbac.ParseFor(t.Window)
		if err != nil {
			return nil, fmt.Errorf("триггер %s: grant.max_window %q: %v", name, t.Window, err)
		}
		t.MaxWindow = d
		if _, err := inventory.ParseSelectors(t.Scope); err != nil {
			return nil, fmt.Errorf("триггер %s: grant.scope: %w", name, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func findTrigger(name string) (*triggerConf, error) {
	confs, err := triggerConfs()
	if err != nil {
		return nil, err
	}
	for i := range confs {
		if confs[i].Name == name {
			return &confs[i], nil
		}
	}
	var known []string
	for _, c := range confs {
		known = append(known, c.Name)
	}
	if len(known) == 0 {
		return nil, fmt.Errorf("триггера %s нет: в config.yaml не задано ни одного (triggers)", name)
	}
	return nil, fmt.Errorf("триггера %s нет; заданы: %s", name, strings.Join(known, ", "))
}

func triggerStore() (*workspace.Triggers, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return workspace.NewTriggers(filepath.Join(fm.BaseDir(), triggersFile)), nil
}

const (
	triggersFile = "triggers.json"
	// triggerSecrets — секреты вебхуков, по файлу на триггер.
	triggerSecrets = "triggers"
)

// settleTriggers закрывает истёкшие окна и пишет об этом в журнал аудита:
// роль погасла сама, и это событие, а не только вывод.
func settleTriggers(store *workspace.Triggers) []workspace.TriggerWindow {
	windows, err := store.Settle(time.Now())
	if err != nil || len(windows) == 0 {
		return nil
	}
	for _, w := range windows {
		auditTrigger(w, fmt.Sprintf("истекло в %s — роль %s снята (max_window %s)",
			w.Until.Local().Format("15:04"), w.Role, w.Window), "")
	}
	return windows
}

// auditTrigger — запись журнала аудита о триггере.
func auditTrigger(w workspace.TriggerWindow, what, remoteAddr string) {
	journal, err := auditJournal()
	if err != nil {
		return
	}
	_ = journal.Append(audit.Record{Event: audit.EventTrigger, Role: w.Role, Command: w.ID(),
		Args: []string{what}, Remote: remoteAddr, User: workspace.CurrentUser()},
		func(audit.Recent) error { return nil })
}

// Окно текущей команды: роль, открытая триггером, и его область.
var (
	activeTrigger string
	triggerScope  []string
)

// triggerGate — роль, которую держит триггер, работает только в окне.
// Заодно ставит окно и область текущей команды.
func triggerGate(role *rbac.Role, key string) error {
	activeTrigger, triggerScope = "", nil
	if role == nil || slices.Contains(rbac.Free, key) {
		return nil
	}
	confs, err := triggerConfs()
	if err != nil {
		return err
	}
	var gating []string
	for _, c := range confs {
		if strings.EqualFold(c.Role, role.Name) {
			gating = append(gating, c.Name)
		}
	}
	if len(gating) == 0 {
		return nil
	}
	store, err := triggerStore()
	if err != nil {
		return err
	}
	settleTriggers(store)
	windows, err := store.Windows()
	if err != nil {
		return err
	}
	open, last := workspace.ForRole(windows, role.Name, time.Now())
	if len(open) == 0 {
		return i18n.Errorf("роль %s действует только в окне триггера (%s): %s", role.Name,
			strings.Join(gating, ", "), roleWindowState(nil, last))
	}
	ids := make([]string, len(open))
	for i, w := range open {
		ids[i] = w.ID()
		if len(w.Scope) == 0 {
			// Окно без области — вся роль; область других окон её не сужает.
			triggerScope = nil
			break
		}
		triggerScope = append(triggerScope, w.Scope...)
	}
	activeTrigger = strings.Join(ids, ",")
	return nil
}

// roleWindowState — «сейчас» роли под триггером.
func roleWindowState(open []workspace.TriggerWindow, last *workspace.TriggerWindow) string {
	if len(open) > 0 {
		parts := make([]string, len(open))
		for i, w := range open {
			parts[i] = fmt.Sprintf("%s, с %s, до %s", w.ID(), w.Start.Local().Format("15:04"), w.Until.Local().Format("15:04"))
		}
		return i18n.Sprintf("активна (%s)", strings.Join(parts, "; "))
	}
	if last == nil {
		return i18n.T("не активна — ждёт события")
	}
	return i18n.Sprintf("не активна — окно %s %s", last.ID(), last.Outcome)
}

// checkTriggerScope — цели сценария в окне триггера только под его
// областью: роль, открытая на prod-релиз, не выкатит на остальные хосты.
func checkTriggerScope(parsed *scenario.Scenario) error {
	if len(triggerScope) == 0 {
		return nil
	}
	sels, err := inventory.ParseSelectors(triggerScope)
	if err != nil {
		return err
	}
	for i := range parsed.Targets {
		t := &parsed.Targets[i]
		if !inventory.MatchAny(sels, t.Labels()) {
			return i18n.Errorf("цель %s вне области окна %s (%s): роль в окне триггера работает только над ней",
				targetName(t, i), activeTrigger, inventory.Describe(sels))
		}
	}
	return nil
}

// scopeHosts — хосты, которые в окне триггера видит inventory() сценария.
func scopeHosts(hosts []inventory.Host) []inventory.Host {
	if len(triggerScope) == 0 || hosts == nil {
		return hosts
	}
	sels, err := inventory.ParseSelectors(triggerScope)
	if err != nil {
		return nil
	}
	return inventory.Filter(hosts, sels)
}

func targetName(t *scenario.Target, i int) string {
	labels := t.Labels()
	for _, key := range []string{"host", "svm_ci", "ip"} {
		if v := labels.Fields[key]; v != "" {
			return v
		}
	}
	return fmt.Sprintf("#%d", i+1)
}

// stampRequest — роль и окно триггера, в которых заявку оформили.
func stampRequest(request *workspace.Request) {
	if currentRole != nil {
		request.Role = currentRole.Name
	}
	request.Trigger = activeTrigger
}

// ---------------------------------------------------------------------------
// Открыть и закрыть окно
// ---------------------------------------------------------------------------

// fireTrigger открывает окно. by — кто прислал событие; remoteAddr — адрес
// вебхука для журнала.
func fireTrigger(name, event, by, remoteAddr string) (*workspace.TriggerWindow, error) {
	conf, err := findTrigger(name)
	if err != nil {
		return nil, err
	}
	if err := checkEventID(event); err != nil {
		return nil, err
	}
	store, roles := (*workspace.Triggers)(nil), (*rbac.Store)(nil)
	if store, err = triggerStore(); err != nil {
		return nil, err
	}
	if roles, _, err = loadRoles(); err != nil {
		return nil, err
	}
	if roles.Find(conf.Role) == nil {
		return nil, fmt.Errorf("триггер %s открывает роль %s, а её нет: ochan role create %s --allow …", name, conf.Role, conf.Role)
	}
	settleTriggers(store)
	now := time.Now()
	w := workspace.TriggerWindow{Trigger: conf.Name, Event: event, Role: conf.Role, Scope: conf.Scope,
		Window: conf.Window, Start: now, Until: now.Add(conf.MaxWindow), By: by}
	if err := store.Fire(w); err != nil {
		return nil, err
	}
	scope := i18n.T("без ограничения целей")
	if len(conf.Scope) > 0 {
		scope = strings.Join(conf.Scope, " | ")
	}
	auditTrigger(w, fmt.Sprintf("→ роль %s выдана: %s, до %s", w.Role, scope, w.Until.Local().Format("15:04")), remoteAddr)
	return &w, nil
}

// endTrigger закрывает окно событием конца.
func endTrigger(name, event, reason, remoteAddr string) (*workspace.TriggerWindow, error) {
	if _, err := findTrigger(name); err != nil {
		return nil, err
	}
	store, err := triggerStore()
	if err != nil {
		return nil, err
	}
	settleTriggers(store)
	if reason == "" {
		reason = "событие конца"
	}
	w, err := store.End(name+"#"+event, "закрыто: "+reason, time.Now())
	if err != nil {
		return nil, err
	}
	auditTrigger(*w, fmt.Sprintf("закрыто — роль %s снята: %s", w.Role, reason), remoteAddr)
	return w, nil
}

var eventID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

func checkEventID(id string) error {
	if !eventID.MatchString(id) {
		return fmt.Errorf("номер события %q: буквы, цифры, «.», «_», «:», «-», до 64 знаков", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Вебхук
// ---------------------------------------------------------------------------

// triggerSecretPath — файл секрета вебхука триггера.
func triggerSecretPath(name string) (string, error) {
	fm, err := mustFileManager()
	if err != nil {
		return "", err
	}
	return filepath.Join(fm.BaseDir(), triggerSecrets, name+".secret"), nil
}

// eventRequest — тело вебхука.
type eventRequest struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// eventsHandler — POST /v1/events/{trigger}: событие CI без клиента ochan.
// Секрет — у каждого триггера свой (ochan trigger secret), запись — только
// на primary: окно — состояние, которое копирует standby.
func eventsHandler(writable func() (bool, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reply := func(code int, text string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"result": text})
		}
		name := r.PathValue("trigger")
		path, err := triggerSecretPath(name)
		if err != nil || !triggerName.MatchString(name) {
			reply(http.StatusNotFound, "нет такого триггера")
			return
		}
		secret, err := os.ReadFile(path)
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if err != nil || len(secret) == 0 ||
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(secret))), []byte(token)) != 1 {
			reply(http.StatusUnauthorized, "нет секрета вебхука или он не тот")
			return
		}
		if ok, why := writable(); !ok {
			reply(http.StatusServiceUnavailable, why)
			return
		}
		var req eventRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			reply(http.StatusBadRequest, "тело — JSON {\"id\", \"state\": \"start\"|\"end\"}")
			return
		}
		addr := remote.ClientIP(r)
		var window *workspace.TriggerWindow
		switch req.State {
		case "start", "":
			window, err = fireTrigger(name, req.ID, "вебхук "+addr, addr)
		case "end":
			window, err = endTrigger(name, req.ID, req.Reason, addr)
		default:
			err = fmt.Errorf("state %q: start или end", req.State)
		}
		if err != nil {
			reply(http.StatusConflict, err.Error())
			return
		}
		reply(http.StatusOK, fmt.Sprintf("%s: роль %s %s", window.ID(), window.Role,
			roleWindowState(openOnly(window), window)))
	}
}

func openOnly(w *workspace.TriggerWindow) []workspace.TriggerWindow {
	if w.Open(time.Now()) {
		return []workspace.TriggerWindow{*w}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Команды
// ---------------------------------------------------------------------------

var (
	triggerEventID string
	triggerReason  string
)

var triggerCmd = &cobra.Command{
	Use:   "trigger",
	Short: "Триггеры: роль, которую открывает событие CI, на окно",
	Long: "Триггер в config.yaml называет событие, роль, которую оно открывает, её\n" +
		"область целей и max_window. Роль под триггером вне окна не может ничего,\n" +
		"кроме того, что доступно любой роли; в окне — свои права, но цели только\n" +
		"под scope. Окно открывает событие (ochan trigger fire или вебхук сервера\n" +
		"POST /v1/events/<триггер>), закрывает событие конца или max_window — сама.\n\n" +
		"Без аргументов — триггеры и их окна.",
	Args: cobra.NoArgs,
	Example: "ochan trigger\n" +
		"ochan trigger fire release --id 1234\n" +
		"ochan trigger end release --id 1234 --reason \"релиз выкачен\"\n" +
		"ochan trigger secret release",
	RunE: func(cmd *cobra.Command, _ []string) error {
		confs, err := triggerConfs()
		if err != nil {
			return err
		}
		if len(confs) == 0 {
			cmd.Println(i18n.T("Триггеров нет: они задаются в config.yaml, раздел triggers"))
			return nil
		}
		store, err := triggerStore()
		if err != nil {
			return err
		}
		settleTriggers(store)
		windows, err := store.Windows()
		if err != nil {
			return err
		}
		now := time.Now()
		for _, c := range confs {
			scope := i18n.T("все цели")
			if len(c.Scope) > 0 {
				scope = strings.Join(c.Scope, " | ")
			}
			cmd.Printf("%-16s событие %s → роль %s, %s, окно до %s\n", c.Name, c.Event, c.Role, scope, c.Window)
			var mine []workspace.TriggerWindow
			for _, w := range windows {
				if w.Trigger == c.Name {
					mine = append(mine, w)
				}
			}
			if len(mine) > 5 {
				mine = mine[len(mine)-5:]
			}
			for _, w := range mine {
				state := w.Outcome
				if w.Open(now) {
					state = i18n.Sprintf("действует до %s", w.Until.Local().Format("15:04"))
				}
				cmd.Printf("  %s  %s  %s — %s\n", w.ID(), w.Start.Local().Format("2006-01-02 15:04"), w.By, state)
			}
		}
		return nil
	},
}

var triggerFireCmd = &cobra.Command{
	Use:   "fire <триггер> --id <номер события>",
	Short: "Открыть окно триггера: событие началось",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		by := workspace.CurrentUser()
		if currentRole != nil {
			by = fmt.Sprintf("%s, роль %s", by, currentRole.Name)
		}
		w, err := fireTrigger(args[0], triggerEventID, by, os.Getenv(EnvRemoteAddr))
		if err != nil {
			return err
		}
		cmd.Printf("Окно %s: роль %s активна до %s", w.ID(), w.Role, w.Until.Local().Format("15:04"))
		if len(w.Scope) > 0 {
			cmd.Printf(", цели — %s", strings.Join(w.Scope, " | "))
		}
		cmd.Println()
		cmd.Printf("Закрыть раньше: ochan trigger end %s --id %s\n", w.Trigger, w.Event)
		return nil
	},
}

var triggerEndCmd = &cobra.Command{
	Use:   "end <триггер> --id <номер события>",
	Short: "Закрыть окно триггера: событие кончилось",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w, err := endTrigger(args[0], triggerEventID, triggerReason, os.Getenv(EnvRemoteAddr))
		if err != nil {
			return err
		}
		cmd.Printf("Окно %s закрыто: роль %s снята\n", w.ID(), w.Role)
		return nil
	},
}

var triggerSecretCmd = &cobra.Command{
	Use:   "secret <триггер>",
	Short: "Создать секрет вебхука триггера",
	Long: "Секрет — 256 случайных бит в ~/.octochan/triggers/<триггер>.secret (права\n" +
		"0600). CI шлёт его заголовком Authorization: Bearer <секрет> на\n" +
		"POST https://<сервер>/v1/events/<триггер> с телом {\"id\": \"1234\",\n" +
		"\"state\": \"start\"} и {\"id\": \"1234\", \"state\": \"end\"}. Новый секрет\n" +
		"заменяет прежний.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := findTrigger(args[0]); err != nil {
			return err
		}
		path, err := triggerSecretPath(args[0])
		if err != nil {
			return err
		}
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(buf)+"\n"), 0o600); err != nil {
			return err
		}
		cmd.Printf("Секрет вебхука: %s (права 0600)\n", path)
		cmd.Printf("CI: curl -X POST -H \"Authorization: Bearer $(cat %s)\" \\\n"+
			"      -d '{\"id\": \"1234\", \"state\": \"start\"}' https://<сервер>/v1/events/%s\n", filepath.Base(path), args[0])
		return nil
	},
}

// requestsUnder — заявки, оформленные в окне триггера.
func requestsUnder(all []*workspace.Request, window string) []*workspace.Request {
	var out []*workspace.Request
	for _, r := range all {
		for _, id := range strings.Split(r.Trigger, ",") {
			if id == window {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

func init() {
	for _, c := range []*cobra.Command{triggerFireCmd, triggerEndCmd} {
		c.Flags().StringVar(&triggerEventID, "id", "", "номер события у источника: номер сборки CI")
		_ = c.MarkFlagRequired("id")
	}
	triggerEndCmd.Flags().StringVar(&triggerReason, "reason", "", "почему окно закрыто")
	triggerCmd.AddCommand(triggerFireCmd, triggerEndCmd, triggerSecretCmd)
	rootCmd.AddCommand(triggerCmd)
}
