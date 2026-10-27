package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"octochan/audit"
	"octochan/rbac"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// EnvRole — переменная окружения, которая задаёт роль процесса. Ею
// ochan-mcp запускает ochan в роли llm: что бы ни было текущей ролью
// пользователя, нейросеть работает в своей.
const EnvRole = "OCTOCHAN_ROLE"

// EnvGrant — допуск сервера на роль из EnvRole: с ним роль процесса
// может быть и защищённой паролем, потому что пароль проверил сервер.
const EnvGrant = "OCTOCHAN_ROLE_GRANT"

var (
	roleAllow     []string
	roleNewLogin  string
	roleNewPsw    bool
	roleRename    string
	roleDropLogin bool
	roleDropPsw   bool
)

// Ввод логина и пароля. Подменяется в тестах.
var (
	roleInput      io.Reader = os.Stdin
	roleInputIsTTY           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
)

func rolesPath() (string, error) {
	fm, err := mustFileManager()
	if err != nil {
		return "", err
	}
	return filepath.Join(fm.BaseDir(), rbac.FileName), nil
}

func loadRoles() (*rbac.Store, string, error) {
	path, err := rolesPath()
	if err != nil {
		return nil, "", err
	}
	store, err := rbac.Load(path)
	if err != nil {
		return nil, "", err
	}
	sys, err := systemPolicy()
	if err != nil {
		return nil, "", err
	}
	if sys != nil {
		// Роли, которые политика положила пользователю, есть всегда, даже
		// если их нет в его хранилище: права — их потолок.
		for _, name := range sys.Allowed(rbac.CurrentIdentity()) {
			if store.Find(name) == nil {
				if role := sys.Materialize(name); role != nil {
					store.Roles = append(store.Roles, role)
				}
			}
		}
	}
	return store, path, nil
}

// systemPolicy — системная политика ролей (rbac/system.go); nil — её нет.
func systemPolicy() (*rbac.SystemPolicy, error) {
	return rbac.LoadSystem(rbac.SystemPolicyPath, moduleCommands())
}

// constrain ставит роли границы системной политики: роль должна быть
// положена пользователю ОС, а права — не выше потолка. Текущая роль из
// хранилища, которая не положена, заменяется первой положенной: хранилище
// — в руках пользователя, политика — нет.
//
// warn — сказать о замене: перед командой да, в показе роли — уже сказано.
func constrain(cmd *cobra.Command, store *rbac.Store, role *rbac.Role, forced, warn bool) (*rbac.Role, error) {
	sys, err := systemPolicy()
	if err != nil || sys == nil {
		return role, err
	}
	id := rbac.CurrentIdentity()
	// Дочерний процесс сервера: роль клиента подтвердил сервер своим
	// допуском, а держать сервер этому пользователю ОС положено.
	if forced && os.Getenv(EnvGrant) != "" && sys.Permits(id, "serve") {
		if _, ok := sys.Roles[role.Name]; ok {
			return sys.Ceil(role), nil
		}
		return role, nil
	}
	if err := sys.Permit(id, role.Name); err != nil {
		allowed := sys.Allowed(id)
		if forced || len(allowed) == 0 {
			return nil, err
		}
		next := store.Find(allowed[0])
		if next == nil {
			return nil, err
		}
		if warn {
			cmd.PrintErrf("внимание: %v — роль %s (ochan set role %s, чтобы не видеть этого)\n", err, next.Name, next.Name)
		}
		role = next
	}
	return sys.Ceil(role), nil
}

// permitRole — переключиться на роль можно, только если она положена
// пользователю системной политикой.
func permitRole(role *rbac.Role) error {
	sys, err := systemPolicy()
	if err != nil || sys == nil {
		return err
	}
	return sys.Permit(rbac.CurrentIdentity(), role.Name)
}

// activeRole — роль, в которой выполняется команда: заданная окружением
// или текущая.
//
// Роль из окружения не должна требовать входа: иначе OCTOCHAN_ROLE=master
// обходил бы пароль master. Поэтому так можно задать только роль без
// логина и пароля — llm и ей подобные.
func activeRole(store *rbac.Store) (*rbac.Role, bool, error) {
	name := os.Getenv(EnvRole)
	if name == "" {
		role := store.CurrentRole()
		if role == nil {
			return nil, false, fmt.Errorf("текущей роли %s нет", store.Current)
		}
		return role, false, nil
	}
	role := store.Find(name)
	if role == nil {
		return nil, true, fmt.Errorf("%s=%s: такой роли нет", EnvRole, name)
	}
	if grant := os.Getenv(EnvGrant); grant != "" {
		if err := store.CheckGrant(role.Name, grant, time.Now()); err != nil {
			return nil, true, fmt.Errorf("%s: %w", EnvGrant, err)
		}
		return role, true, nil
	}
	if role.NeedsAuth() {
		return nil, true, fmt.Errorf("%s=%s: роль защищена логином или паролем, задать её окружением нельзя; "+
			"войдите командой set role %s", EnvRole, role.Name, role.Name)
	}
	return role, true, nil
}

// commandKey — путь команды через точку без имени программы: diff,
// fp.check, role.create.
func commandKey(cmd *cobra.Command) string {
	var parts []string
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		parts = append([]string{c.Name()}, parts...)
	}
	return strings.Join(parts, ".")
}

// moduleCommands — команды, которых нет ни в одной группе прав: команды
// подключённых модулей. Их можно выдать по имени; право на команду модуля
// открывает и её подкоманды. Свои команды с подкомандами (set, back, tls)
// по имени не выдаются: их подкоманды уже распределены по группам.
func moduleCommands() []string {
	known := map[string]bool{}
	for _, name := range append(rbac.KnownCommands(), rbac.Free...) {
		known[name] = true
	}
	var extra []string
	for _, c := range rootCmd.Commands() {
		if known[c.Name()] {
			continue
		}
		if c.Annotations[moduleCommandAnnotation] == "module" || !c.HasSubCommands() {
			extra = append(extra, c.Name())
		}
	}
	sort.Strings(extra)
	return extra
}

// authorize проверяет права текущей роли перед каждой командой —
// и в командной строке, и в оболочке — и пишет решение в журнал аудита.
func authorize(cmd *cobra.Command, args []string) error {
	currentRole = nil
	key := commandKey(cmd)
	if key == "" {
		return nil // сама программа: оболочка или справка
	}

	store, path, err := loadRoles()
	if err != nil {
		return err
	}
	if os.Getenv(EnvRole) == "" {
		if err := expireRole(cmd, store, path); err != nil {
			return err
		}
	}
	role, forced, err := activeRole(store)
	if err == nil {
		role, err = constrain(cmd, store, role, forced, true)
	}
	if err != nil {
		// Роль из окружения не прошла: несуществующая роль или чужой
		// допуск — это тоже то, что журнал должен помнить.
		if journal, jerr := auditJournal(); jerr == nil && !unaudited(key) {
			_ = journal.Append(audit.Record{Event: audit.EventCommand, Role: os.Getenv(EnvRole), Command: key,
				Reason: err.Error(), Remote: os.Getenv(EnvRemoteAddr), User: workspace.CurrentUser()},
				func(audit.Recent) error { return err })
		}
		return err
	}
	if err := admit(cmd, role, key, args); err != nil {
		return err
	}
	currentRole = role
	return nil
}

// requiredPerm называет право, которое открыло бы команду.
func requiredPerm(key string) string {
	top := key
	if i := strings.IndexByte(key, '.'); i > 0 {
		top = key[:i]
	}
	for _, group := range rbac.GroupNames() {
		for _, member := range rbac.Groups[group] {
			if member == key || member == top {
				return group
			}
		}
	}
	return key + " или " + rbac.All
}

// ---------------------------------------------------------------------------
// Ввод логина и пароля
// ---------------------------------------------------------------------------

type credentialReader struct {
	cmd   *cobra.Command
	lines *bufio.Reader
}

func newCredentialReader(cmd *cobra.Command) *credentialReader {
	return &credentialReader{cmd: cmd, lines: bufio.NewReader(roleInput)}
}

func (r *credentialReader) line(prompt string) (string, error) {
	r.cmd.Print(prompt)
	text, err := r.lines.ReadString('\n')
	switch {
	case errors.Is(err, io.EOF) && text == "":
		return "", errors.New("ввод закончился, а ответа нет: логин или пароль не введён")
	case err != nil && !errors.Is(err, io.EOF):
		return "", fmt.Errorf("не удалось прочитать ввод: %w", err)
	}
	return strings.TrimRight(text, "\r\n"), nil
}

// secret читает пароль без эха. Не терминал — одна строка из stdin.
func (r *credentialReader) secret(prompt string) (string, error) {
	if !roleInputIsTTY() {
		return r.line(prompt)
	}
	r.cmd.Print(prompt + "(ввод скрыт) ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	r.cmd.Println()
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать пароль: %w", err)
	}
	return string(raw), nil
}

// newPassword читает новый пароль; на терминале — дважды.
func (r *credentialReader) newPassword(role string) (string, error) {
	password, err := r.secret(fmt.Sprintf("Новый пароль роли %s: ", role))
	if err != nil {
		return "", err
	}
	if password == "" {
		return "", errors.New("пустой пароль: чтобы снять пароль, используйте --drop-psw")
	}
	if roleInputIsTTY() {
		again, err := r.secret("Повторите пароль: ")
		if err != nil {
			return "", err
		}
		if again != password {
			return "", errors.New("пароли не совпадают")
		}
	}
	return password, nil
}

// ---------------------------------------------------------------------------
// Команды
// ---------------------------------------------------------------------------

var roleCmd = &cobra.Command{
	Use:   "role",
	Short: "Роли и права: текущая роль, список, создание и правка",
	Long: "Без подкоманды показывает текущую роль и историю переключений.\n\n" +
		"После установки есть две роли: master — первая и высшая, ей доступно всё;\n" +
		"llm — для нейросетей: читать, планировать, оформлять заявки. Права — группы\n" +
		"(" + strings.Join(rbac.GroupNames(), ", ") + "), all или имя команды.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		store, _, err := loadRoles()
		if err != nil {
			return err
		}
		role, forced, err := activeRole(store)
		if err == nil {
			role, err = constrain(cmd, store, role, forced, false)
		}
		if err != nil {
			return err
		}
		cmd.Printf("Текущая роль: %s\n", role.Name)
		if forced {
			cmd.Printf("Задана окружением %s; переключение недоступно\n", EnvRole)
			return nil
		}
		if !store.CurrentUntil.IsZero() {
			cmd.Printf("До:           %s, затем %s\n", formatUntil(store.CurrentUntil, rbac.Now()), store.ReturnTo)
		}
		cmd.Printf("История:      %s\n", strings.Join(store.History, " -> "))
		return nil
	},
}

var roleListCmd = &cobra.Command{
	Use:   "list",
	Short: "Перечислить роли и их права",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		store, _, err := loadRoles()
		if err != nil {
			return err
		}
		active, _, err := activeRole(store)
		if err != nil {
			return err
		}
		for _, role := range store.Roles {
			marker := " "
			if role == active {
				marker = "*"
			}
			var notes []string
			if role.Builtin {
				notes = append(notes, "встроенная")
			}
			if role.Login != nil {
				notes = append(notes, "логин")
			}
			if role.Password != nil {
				notes = append(notes, "пароль")
			}
			if len(role.Deny) > 0 {
				notes = append(notes, "запреты: "+strings.Join(role.Deny, ","))
			}
			if role.Scoped() {
				notes = append(notes, "область")
			}
			if _, fromConfig, _ := effectiveLimits(role); len(role.Limits) > 0 || len(fromConfig) > 0 {
				notes = append(notes, "пределы")
			}
			if temp := role.ActiveTemp(); len(temp) > 0 {
				notes = append(notes, "на время: "+strings.Join(tempPermNames(temp), ","))
			}
			if temp := role.ActiveTempDeny(); len(temp) > 0 {
				notes = append(notes, "запреты на время: "+strings.Join(tempPermNames(temp), ","))
			}
			cmd.Printf("%s %-16s %-32s %s\n", marker, role.Name, strings.Join(role.Perms, ", "), strings.Join(notes, ", "))
		}
		return nil
	},
}

var roleCreateCmd = &cobra.Command{
	Use:   "create <роль> --allow <права>",
	Short: "Создать роль и выдать ей права",
	Args:  cobra.ExactArgs(1),
	Example: "ochan role create slave --allow read,plan\n" +
		"ochan role create Dan --allow read,plan,request,approve --nlogin dan --npsw\n" +
		"ochan role create ops --allow all --deny serve,admin",
	RunE: func(cmd *cobra.Command, args []string) error {
		return mutateRoles(func(store *rbac.Store) error {
			role, err := store.Create(args[0], splitPerms(roleAllow), moduleCommands())
			if err != nil {
				return err
			}
			if len(roleDeny) > 0 {
				if err := store.DenyPerms(role.Name, splitPerms(roleDeny), moduleCommands()); err != nil {
					return err
				}
			}
			if err := applyCredentialFlags(cmd, store, role.Name); err != nil {
				return err
			}
			cmd.Printf("Роль %s создана: %s\n", role.Name, strings.Join(role.Perms, ", "))
			if len(role.Deny) > 0 {
				cmd.Printf("Запрещено: %s\n", strings.Join(role.Deny, ", "))
			}
			return nil
		})
	},
}

var roleDeleteCmd = &cobra.Command{
	Use:   "delete <роль>",
	Short: "Удалить роль",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mutateRoles(func(store *rbac.Store) error {
			if err := store.Delete(args[0]); err != nil {
				return err
			}
			cmd.Printf("Роль %s удалена\n", args[0])
			return nil
		})
	},
}

var grantTemp tempFlags

var roleGrantCmd = &cobra.Command{
	Use:   "grant <роль> <право>... [--for <срок> | --until <момент>]",
	Short: "Выдать роли права, навсегда или на время",
	Long: "С --for или --until право действует до срока и пропадает само:\n" +
		"отзывать нечего и забыть отозвать нельзя. Выдать на время может и CI\n" +
		"или скрипт — той же командой, в роли с правом admin; каждая выдача —\n" +
		"в журнале аудита.",
	Args: cobra.MinimumNArgs(2),
	Example: "ochan role grant slave approve\nochan role grant slave patch rollback\n" +
		"ochan role grant dan apply --for 2h\nochan role grant dan apply --until \"2026-10-24 18:00\"",
	RunE: func(cmd *cobra.Command, args []string) error {
		now := rbac.Now()
		until, temporary, err := grantTemp.until(now)
		if err != nil {
			return err
		}
		return mutateRoles(func(store *rbac.Store) error {
			perms := splitPerms(args[1:])
			if temporary {
				if err := store.GrantFor(args[0], perms, until, moduleCommands()); err != nil {
					return err
				}
				cmd.Printf("Роль %s: %s до %s\n", store.Find(args[0]).Name, strings.Join(perms, ", "), formatUntil(until, now))
				return nil
			}
			if err := store.Grant(args[0], perms, moduleCommands()); err != nil {
				return err
			}
			cmd.Printf("Роль %s: %s\n", store.Find(args[0]).Name, strings.Join(store.Find(args[0]).Perms, ", "))
			return nil
		})
	},
}

var roleRevokeCmd = &cobra.Command{
	Use:   "revoke <роль> <право>...",
	Short: "Забрать у роли права",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mutateRoles(func(store *rbac.Store) error {
			if err := store.Revoke(args[0], splitPerms(args[1:])); err != nil {
				return err
			}
			cmd.Printf("Роль %s: %s\n", store.Find(args[0]).Name, strings.Join(store.Find(args[0]).Perms, ", "))
			return nil
		})
	},
}

var roleUpdateCmd = &cobra.Command{
	Use:   "update <роль> [--nlogin <логин>] [--npsw] [--rename <имя>]",
	Short: "Сменить логин, пароль или имя роли",
	Long: "--nlogin задаёт новый логин, --npsw спрашивает новый пароль (ввод скрыт;\n" +
		"не на терминале — одна строка из stdin). --drop-login и --drop-psw снимают\n" +
		"логин и пароль: в роль снова можно входить без них.\n\n" +
		"После смены логина или пароля роль уходит из истории переключений:\n" +
		"back role в неё больше не пустит без входа.",
	Args: cobra.ExactArgs(1),
	Example: "ochan role update master --npsw\n" +
		"ochan role update slave --nlogin dan --npsw\n" +
		"ochan role update slave --rename worker",
	RunE: func(cmd *cobra.Command, args []string) error {
		if roleNewLogin == "" && !roleNewPsw && roleRename == "" && !roleDropLogin && !roleDropPsw {
			return errors.New("укажите, что менять: --nlogin, --npsw, --rename, --drop-login или --drop-psw")
		}
		return mutateRoles(func(store *rbac.Store) error {
			role := store.Find(args[0])
			if role == nil {
				return fmt.Errorf("роли %s нет", args[0])
			}
			name := role.Name

			if roleDropLogin {
				if err := store.SetLogin(name, nil); err != nil {
					return err
				}
			}
			if roleDropPsw {
				if err := store.SetPassword(name, nil); err != nil {
					return err
				}
			}
			if err := applyCredentialFlags(cmd, store, name); err != nil {
				return err
			}
			if roleRename != "" {
				if err := store.Rename(name, roleRename); err != nil {
					return err
				}
				name = roleRename
			}
			cmd.Printf("Роль %s обновлена\n", name)
			return nil
		})
	},
}

// applyCredentialFlags применяет --nlogin и --npsw.
func applyCredentialFlags(cmd *cobra.Command, store *rbac.Store, name string) error {
	if roleNewLogin != "" && roleDropLogin {
		return errors.New("--nlogin и --drop-login вместе: непонятно, задать логин или снять")
	}
	if roleNewPsw && roleDropPsw {
		return errors.New("--npsw и --drop-psw вместе: непонятно, задать пароль или снять")
	}
	if roleNewLogin != "" {
		login := roleNewLogin
		if err := store.SetLogin(name, &login); err != nil {
			return err
		}
	}
	if roleNewPsw {
		password, err := newCredentialReader(cmd).newPassword(name)
		if err != nil {
			return err
		}
		if err := store.SetPassword(name, &password); err != nil {
			return err
		}
	}
	return nil
}

var setCmd = &cobra.Command{
	Use:   "set",
	Short: "Переключение: set role <роль>",
}

var setRoleTemp tempFlags

var setRoleCmd = &cobra.Command{
	Use:   "role <роль> [--for <срок> | --until <момент>]",
	Short: "Переключиться на роль, навсегда или на время",
	Long: "Если у роли задан логин или пароль, их спросят. В роль, в которую\n" +
		"уже входили, можно вернуться без них: back role.\n\n" +
		"С --for или --until роль действует до срока, а потом сама возвращается\n" +
		"прежняя: поднять права на полчаса и не забыть их опустить.",
	Args:    cobra.ExactArgs(1),
	Example: "ochan set role slave\nochan set role Dan\nochan set role master --for 30m",
	RunE: func(cmd *cobra.Command, args []string) error {
		now := rbac.Now()
		until, temporary, err := setRoleTemp.until(now)
		if err != nil {
			return err
		}
		if temporary {
			return switchRoleFor(cmd, args[0], until, now)
		}
		return switchRole(cmd, func(store *rbac.Store) (*rbac.Role, error) {
			role := store.Find(args[0])
			if role == nil {
				return nil, fmt.Errorf("роли %s нет", args[0])
			}
			if err := permitRole(role); err != nil {
				return nil, err
			}
			if role.Name == store.Current {
				return nil, fmt.Errorf("роль %s уже текущая", role.Name)
			}
			if err := login(cmd, role); err != nil {
				return nil, err
			}
			return role, nil
		})
	},
}

var backCmd = &cobra.Command{
	Use:   "back",
	Short: "Возврат: back role [роль]",
}

var backRoleCmd = &cobra.Command{
	Use:   "role [роль]",
	Short: "Вернуться к прошлой роли",
	Long: "Без имени — к роли, которая была текущей до последнего переключения.\n" +
		"Два back role подряд возвращают туда же, откуда начали: set role slave,\n" +
		"back role — master, back role — снова slave. С именем — к этой роли, если\n" +
		"она есть в истории. Логин и пароль не спрашиваются: в роль уже входили.",
	Args:    cobra.MaximumNArgs(1),
	Example: "ochan back role\nochan back role master\nochan back role Dan",
	RunE: func(cmd *cobra.Command, args []string) error {
		target := ""
		if len(args) == 1 {
			target = args[0]
		}
		return switchRole(cmd, func(store *rbac.Store) (*rbac.Role, error) {
			return store.BackTarget(target)
		})
	},
}

// switchRole переключает роль, если она не задана окружением.
func switchRole(cmd *cobra.Command, pick func(*rbac.Store) (*rbac.Role, error)) error {
	if name := os.Getenv(EnvRole); name != "" {
		return fmt.Errorf("роль задана окружением %s=%s: переключение недоступно", EnvRole, name)
	}
	return mutateRoles(func(store *rbac.Store) error {
		role, err := pick(store)
		if err != nil {
			return err
		}
		if err := permitRole(role); err != nil {
			return err
		}
		if err := store.Switch(role.Name); err != nil {
			return err
		}
		cmd.Printf("Роль: %s\n", role.Name)
		return nil
	})
}

// switchRoleFor переключает роль до срока.
func switchRoleFor(cmd *cobra.Command, name string, until, now time.Time) error {
	if env := os.Getenv(EnvRole); env != "" {
		return fmt.Errorf("роль задана окружением %s=%s: переключение недоступно", EnvRole, env)
	}
	return mutateRoles(func(store *rbac.Store) error {
		role := store.Find(name)
		if role == nil {
			return fmt.Errorf("роли %s нет", name)
		}
		if err := permitRole(role); err != nil {
			return err
		}
		if role.Name == store.Current {
			return fmt.Errorf("роль %s уже текущая", role.Name)
		}
		if err := login(cmd, role); err != nil {
			return err
		}
		if err := store.SwitchFor(role.Name, until); err != nil {
			return err
		}
		cmd.Printf("Роль: %s до %s, затем %s\n", role.Name, formatUntil(until, now), store.ReturnTo)
		return nil
	})
}

// login спрашивает логин и пароль, если они заданы у роли.
func login(cmd *cobra.Command, role *rbac.Role) error {
	if !role.NeedsAuth() {
		return nil
	}
	reader := newCredentialReader(cmd)
	var name, password string
	var err error
	if role.Login != nil {
		if name, err = reader.line("Логин: "); err != nil {
			return err
		}
	}
	if role.Password != nil {
		if password, err = reader.secret("Пароль: "); err != nil {
			return err
		}
	}
	// Что именно не совпало, не говорится: подбирать по одному полю
	// проще, чем по двум.
	if !role.Verify(name, password) {
		return fmt.Errorf("неверный логин или пароль для роли %s", role.Name)
	}
	return nil
}

// mutateRoles читает хранилище, меняет и записывает его.
func mutateRoles(change func(*rbac.Store) error) error {
	store, path, err := loadRoles()
	if err != nil {
		return err
	}
	if err := change(store); err != nil {
		return err
	}
	store.Prune(rbac.Now())
	return store.Save(path)
}

func splitPerms(values []string) []string {
	var perms []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				perms = append(perms, part)
			}
		}
	}
	return perms
}

func tempPermNames(list []rbac.TempPerm) []string {
	names := make([]string, len(list))
	for i, t := range list {
		names[i] = t.Perm
	}
	return names
}

func init() {
	grantTemp.add(roleGrantCmd, "выдать")
	setRoleTemp.add(setRoleCmd, "переключиться")
	roleCreateCmd.Flags().StringSliceVar(&roleAllow, "allow", nil, "права роли через запятую")
	for _, c := range []*cobra.Command{roleCreateCmd, roleUpdateCmd} {
		c.Flags().StringVar(&roleNewLogin, "nlogin", "", "задать логин роли")
		c.Flags().BoolVar(&roleNewPsw, "npsw", false, "задать пароль роли (спросит, ввод скрыт)")
	}
	roleUpdateCmd.Flags().StringVar(&roleRename, "rename", "", "новое имя роли")
	roleUpdateCmd.Flags().BoolVar(&roleDropLogin, "drop-login", false, "снять логин")
	roleUpdateCmd.Flags().BoolVar(&roleDropPsw, "drop-psw", false, "снять пароль")

	roleCmd.AddCommand(roleListCmd, roleCreateCmd, roleDeleteCmd, roleGrantCmd, roleRevokeCmd, roleUpdateCmd)
	setCmd.AddCommand(setRoleCmd)
	backCmd.AddCommand(backRoleCmd)
}
