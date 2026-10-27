package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"octochan/audit"
	"octochan/i18n"
	"octochan/lang"
	"octochan/rbac"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// EnvRemoteAddr — адрес клиента сервера; его передаёт дочернему процессу
// сервер, и он попадает в журнал аудита.
const EnvRemoteAddr = "OCTOCHAN_REMOTE_ADDR"

// currentRole — роль, в которой выполняется текущая команда; её ставит
// authorize. Команды, которым роль важна (согласование, заявки), берут
// её отсюда, а не ищут заново.
var currentRole *rbac.Role

// remoteChild сообщает, что команду прислал клиент сервера: роль процессу
// дал допуск сервера.
func remoteChild() bool { return os.Getenv(EnvGrant) != "" }

func auditJournal() (*audit.Journal, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return &audit.Journal{Path: filepath.Join(fm.BaseDir(), "audit.log")}, nil
}

// unaudited — команды, которые не пишутся в журнал: справка и
// автодополнение оболочки, которое вызывается на каждое нажатие Tab.
func unaudited(key string) bool {
	return key == "help" || strings.HasPrefix(key, "completion") || strings.HasPrefix(key, "__complete")
}

// refusal — отказ политики, в отличие от сбоя журнала.
type refusal struct{ err error }

func (r refusal) Error() string { return r.err.Error() }
func (r refusal) Unwrap() error { return r.err }

// admit решает, может ли роль выполнить команду, и пишет решение в
// журнал аудита. Предел частоты считается по тому же журналу под его
// замком: два процесса не пройдут оба по последней единице предела.
func admit(cmd *cobra.Command, role *rbac.Role, key string, args []string) error {
	decision := checkCommand(cmd, role, key, args)
	if unaudited(key) {
		return decision
	}

	journal, err := auditJournal()
	if err == nil {
		rec := audit.Record{Event: audit.EventCommand, Role: role.Name, Command: key,
			Args: auditArgs(cmd, args), Remote: os.Getenv(EnvRemoteAddr), User: workspace.CurrentUser()}
		if strings.HasPrefix(key, "set.role") || strings.HasPrefix(key, "back.role") {
			rec.Event = audit.EventRole
		}
		err = journal.Append(rec, func(recent audit.Recent) error {
			if decision != nil {
				return refusal{decision}
			}
			if err := checkLimits(role, key, recent); err != nil {
				return refusal{err}
			}
			return nil
		})
	}

	var refused refusal
	switch {
	case err == nil:
		return nil
	case errors.As(err, &refused):
		return refused.err
	case decision != nil:
		return decision
	case role.Name == rbac.Master && len(role.Limits) == 0:
		// Владелец без журнала работает: иначе испорченный журнал
		// запирал бы его на собственной машине. Но молча — нет.
		cmd.PrintErrf("%s\n", i18n.Sprintf("внимание: журнал аудита недоступен: %v", err))
		return nil
	default:
		return i18n.Errorf("журнал аудита недоступен, а роль %s без него не работает: %v", role.Name, err)
	}
}

// refuse пишет в журнал аудита отказ, который команда вынесла сама, уже
// после допуска: свою заявку не согласуют, заявка вне области. Иначе в
// журнале такая попытка выглядела бы разрешённой.
func refuse(cmd *cobra.Command, err error) error {
	if err == nil || currentRole == nil {
		return err
	}
	if journal, jerr := auditJournal(); jerr == nil {
		_ = journal.Append(audit.Record{Event: audit.EventCommand, Role: currentRole.Name,
			Command: commandKey(cmd), Args: auditArgs(cmd, cmd.Flags().Args()),
			Remote: os.Getenv(EnvRemoteAddr), User: workspace.CurrentUser()},
			func(audit.Recent) error { return err })
	}
	return err
}

// checkCommand — права, запреты и пути команды.
func checkCommand(cmd *cobra.Command, role *rbac.Role, key string, args []string) error {
	if role.Denies(key) {
		return i18n.Errorf("команда %s роли %s запрещена", strings.ReplaceAll(key, ".", " "), role.Name)
	}
	if !role.Allows(key) {
		return i18n.Errorf("роль %s не может выполнять %s: нужно право %s", role.Name,
			strings.ReplaceAll(key, ".", " "), requiredPerm(key))
	}
	if key == "eval" && len(scopeOf(role)) > 0 {
		return i18n.Errorf("eval выполняет язык без песочницы, а роль %s ограничена каталогами: "+
			"используйте run --sandbox", role.Name)
	}
	return checkPaths(cmd, role, key, args)
}

// scopeOf — область роли. На сервере роль без своей области работает в
// каталоге сервера: клиент, которому дали apply, иначе дотянулся бы до
// любого файла машины.
func scopeOf(role *rbac.Role) []string {
	if role == nil || role.Name == rbac.Master {
		return nil
	}
	if role.Scoped() {
		return role.Paths
	}
	if remoteChild() {
		if cwd, err := os.Getwd(); err == nil {
			if real, err := lang.RealPath(cwd); err == nil {
				return []string{real}
			}
			return []string{cwd}
		}
	}
	return nil
}

// protectedPaths — рабочий каталог ochan и то, что лежит вне его, но так
// же важно: конфиг, указанный окружением, и ключ TLS сервера.
func protectedPaths() []string {
	fm, err := mustFileManager()
	if err != nil {
		return nil
	}
	paths := []string{fm.BaseDir(), configPath(fm)}
	if key := viper.GetString("tls.key"); key != "" {
		paths = append(paths, key)
	}
	var real []string
	for _, path := range paths {
		if resolved, err := lang.RealPath(path); err == nil {
			path = resolved
		}
		real = append(real, path)
	}
	return real
}

// pathFree — команды, аргументы которых не пути этой машины.
var pathFree = map[string]bool{"connect": true, "remote": true}

// checkPaths не пускает роль за пределы области и в рабочий каталог
// ochan. Путём считается любой аргумент и любое значение строкового
// флага: относительный путь лежит в текущем каталоге, а он сам — в
// области. Наружу выводят только абсолютные пути, «..» и символические
// ссылки, а их раскрывает RealPath.
func checkPaths(cmd *cobra.Command, role *rbac.Role, key string, args []string) error {
	if role.Name == rbac.Master || pathFree[key] {
		return nil
	}
	scope := scopeOf(role)
	scoped := &rbac.Role{Name: role.Name, Paths: scope}
	protected := protectedPaths()

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("не удалось определить текущий каталог: %w", err)
	}
	if real, err := lang.RealPath(cwd); err == nil {
		cwd = real
	}
	if !scoped.InScope(cwd) {
		return i18n.Errorf("текущий каталог %s вне области роли %s (%s)", cwd, role.Name, strings.Join(scope, ", "))
	}

	for _, value := range pathCandidates(cmd, args) {
		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		real, err := lang.RealPath(filepath.Clean(path))
		if err != nil {
			return i18n.Errorf("путь %s не проверить: %v", value, err)
		}
		for _, dir := range protected {
			if rbac.Inside(dir, real) {
				return i18n.Errorf("%s — рабочий каталог ochan: роли %s он закрыт", value, role.Name)
			}
		}
		if !scoped.InScope(real) {
			return i18n.Errorf("%s вне области роли %s (%s)", value, role.Name, strings.Join(scope, ", "))
		}
	}
	return nil
}

// pathCandidates — аргументы и значения строковых флагов, кроме «-» и
// ссылок на живые источники.
func pathCandidates(cmd *cobra.Command, args []string) []string {
	values := append([]string(nil), args...)
	visitChanged(cmd, func(f *pflag.Flag) {
		switch f.Value.Type() {
		case "string":
			values = append(values, f.Value.String())
		case "stringArray", "stringSlice":
			if slice, ok := f.Value.(pflag.SliceValue); ok {
				values = append(values, slice.GetSlice()...)
			}
		}
	})
	var out []string
	for _, value := range values {
		if value == "" || value == "-" || strings.Contains(value, "://") {
			continue
		}
		out = append(out, value)
	}
	return out
}

// checkLimits проверяет пределы частоты по недавним записям журнала.
func checkLimits(role *rbac.Role, key string, recent audit.Recent) error {
	all, _, err := effectiveLimits(role)
	if err != nil {
		return err
	}
	limited := *role
	limited.Limits = all
	limits := limited.LimitsFor(key)
	if len(limits) == 0 {
		return nil
	}
	now := time.Now()
	records, err := recent(now.Add(-limited.MaxWindow()))
	if err != nil {
		return err
	}
	for _, limit := range limits {
		used := 0
		var oldest time.Time
		for _, rec := range records {
			if rec.Event != audit.EventCommand || !rec.Allowed || !strings.EqualFold(rec.Role, role.Name) ||
				rec.Time.Before(now.Add(-limit.Window)) || !rbac.Covers(limit.Perm, rec.Command) {
				continue
			}
			if used == 0 {
				oldest = rec.Time
			}
			used++
		}
		if used >= limit.Count {
			wait := oldest.Add(limit.Window).Sub(now).Round(time.Second)
			return i18n.Errorf("предел роли %s исчерпан: %s; следующая попытка через %s", role.Name, limit, wait)
		}
	}
	return nil
}

// effectiveLimits — пределы роли: из хранилища ролей (role limit) и из
// config.yaml (roles.<роль>.limits, в той же записи: plan=60/1h).
// Предел хранилища для того же права важнее: его задали этой роли командой.
// fromConfig — пределы, которые пришли из конфига. У master пределов не
// бывает: опечатка в конфиге не должна запереть владельца.
//
// Так задаётся частота plan для llm: модель, которой подсунули вредную
// инструкцию, не построит сотню планов в минуту, а значение — настройка,
// а не код.
func effectiveLimits(role *rbac.Role) (all, fromConfig []rbac.Limit, err error) {
	all = append(all, role.Limits...)
	if strings.EqualFold(role.Name, rbac.Master) {
		return all, nil, nil
	}
	key := "roles." + strings.ToLower(role.Name) + ".limits"
	for _, text := range viper.GetStringSlice(key) {
		limit, err := rbac.ParseLimit(text)
		if err != nil {
			return nil, nil, fmt.Errorf("config.yaml %s: %w", key, err)
		}
		if err := rbac.CheckPerms([]string{limit.Perm}, moduleCommands()); err != nil {
			return nil, nil, fmt.Errorf("config.yaml %s: %w", key, err)
		}
		if slices.ContainsFunc(role.Limits, func(l rbac.Limit) bool { return l.Perm == limit.Perm }) {
			continue
		}
		all = append(all, limit)
		fromConfig = append(fromConfig, limit)
	}
	return all, fromConfig, nil
}

// visitChanged обходит флаги, заданные в этой команде. Visit из pflag
// помнит и флаги прошлых команд того же процесса — в оболочке ochan
// shell они попали бы в журнал и в проверку области.
func visitChanged(cmd *cobra.Command, fn func(*pflag.Flag)) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Changed {
			fn(f)
		}
	})
}

// auditArgs — аргументы команды для журнала, без паролей.
func auditArgs(cmd *cobra.Command, args []string) []string {
	var out []string
	visitChanged(cmd, func(f *pflag.Flag) {
		value := f.Value.String()
		if f.Value.Type() == "bool" {
			out = append(out, "--"+f.Name)
			return
		}
		out = append(out, "--"+f.Name+"="+redactSecrets(value))
	})
	for _, arg := range args {
		out = append(out, redactSecrets(arg))
	}
	return out
}

// checkRequestScope проверяет, что заявка не выходит за область роли.
// Сценарий без песочницы может тронуть что угодно, поэтому роль с
// областью такую заявку не согласует и не выполнит.
func checkRequestScope(role *rbac.Role, request *workspace.Request) error {
	if role == nil {
		return nil
	}
	scope := scopeOf(role)
	if len(scope) == 0 {
		return nil
	}
	scoped := &rbac.Role{Name: role.Name, Paths: scope}
	var paths []string
	switch {
	case request.Script != nil:
		if !request.Script.Sandboxed {
			return i18n.Errorf("заявка %s — сценарий без песочницы, а роль %s ограничена каталогами", request.ID, role.Name)
		}
		paths = append(append(paths, request.Script.Roots...), request.Script.Dir)
	case request.Service == "" && request.Target != "":
		if strings.Contains(request.Target, "://") {
			return i18n.Errorf("заявка %s меняет живой источник, а роль %s ограничена каталогами", request.ID, role.Name)
		}
		paths = append(paths, request.Target)
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		real, err := lang.RealPath(path)
		if err != nil {
			real = path
		}
		if !scoped.InScope(real) {
			return i18n.Errorf("заявка %s касается %s — вне области роли %s", request.ID, path, role.Name)
		}
	}
	return nil
}

// checkEntryScope проверяет, что откат не выходит за область роли.
func checkEntryScope(role *rbac.Role, entry *workspace.Entry) error {
	if role == nil {
		return nil
	}
	scope := scopeOf(role)
	if len(scope) == 0 {
		return nil
	}
	scoped := &rbac.Role{Name: role.Name, Paths: scope}
	paths := []string{entry.Target}
	for _, file := range entry.Files {
		paths = append(paths, file.Path)
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		real, err := lang.RealPath(path)
		if err != nil {
			real = path
		}
		if !scoped.InScope(real) {
			return i18n.Errorf("запись %s касается %s — вне области роли %s", entry.ID, path, role.Name)
		}
	}
	return nil
}

// checkSandboxRoots не даёт песочнице сценария накрыть рабочий каталог
// ochan: ядро пустило бы сценарий в него целиком. Области роли каталоги
// песочницы и так не покидают: --root — значение флага, и его проверила
// checkPaths.
func checkSandboxRoots(role *rbac.Role, roots []string) error {
	if role == nil || role.Name == rbac.Master {
		return nil
	}
	for _, root := range roots {
		for _, dir := range protectedPaths() {
			if rbac.Inside(root, dir) {
				return i18n.Errorf("каталог песочницы %s содержит рабочий каталог ochan %s: роли %s так нельзя, "+
					"укажите каталог проекта", root, dir, role.Name)
			}
		}
	}
	return nil
}
