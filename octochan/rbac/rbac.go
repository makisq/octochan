// Package rbac — роли octochan и их права.
//
// После установки есть две роли. master — первая и высшая, ей доступно
// всё; на своём компьютере ты master. llm — роль для нейросетей: читать,
// планировать и оформлять заявки, но не выполнять и не согласовывать.
// Остальные роли создаёт master и выдаёт им права.
//
// Локально хранилище ролей — защита от ошибки и граница для нейросети, а
// не от владельца компьютера: кто может удалить ~/.octochan, тот вернёт
// роли к исходным. Роль, которую подтверждает не рабочий каталог, а файл
// root, — системная политика (system.go). Границей против чужих роли
// становятся и на общем сервере, где хранилище лежит у сервера, а не у
// того, кто подключается.
package rbac

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Имена встроенных ролей.
const (
	Master = "master"
	LLM    = "llm"
)

// All — все права, включая команды, которых нет ни в одной группе.
const All = "all"

// Groups — группы прав. Право — имя группы или имя команды. Вложенная
// команда пишется через точку: role.create.
var Groups = map[string][]string{
	"read": {"agents", "aliases", "check", "diff", "else", "find", "fp", "ha", "history", "if", "inventory", "lang",
		"logs", "modules", "policy", "print", "requests", "sandbox", "services", "stats", "status", "trigger", "validate",
		// !!search только читает реестры из настроек.
		"!!search"},
	"plan": {"plan"},
	// compensate оформляет обратное действие заявкой: его согласуют, как
	// любую другую.
	// watch оформляет заявки; выполнить по политике он может, только если у
	// роли есть и apply — это проверяется как у команды apply.
	"request": {"compensate", "request", "watch"},
	// inventory.ack — решение, что новый сервер уже не новый: ochan watch
	// больше не построит для него план. policy.add — заранее данное
	// согласование: по политике watch согласует заявки сам.
	// fp.journal.amend — поздняя поправка исхода плана: тоже решение.
	"approve": {"agents.forget", "approve", "fp.journal.amend", "inventory.ack", "policy.add", "policy.revoke", "reject"},
	// agent — машина-агент: сообщать серверу о себе и только.
	"agent": {"agents.beat"},
	// trigger — источник событий (CI): открыть и закрыть окно триггера, и
	// только. Какую роль открывает окно, решает config.yaml, а не он.
	"trigger": {"trigger.end", "trigger.fire"},
	"apply":   {"apply", "eval", "patch", "rollback", "run", "srs", "tee"},
	"install": {"!!install", "!!registry", "!!rin", "inspect"},
	"auth":    {"auth", "timer"},
	"admin": {"role.create", "role.delete", "role.deny", "role.grant", "role.limit", "role.revoke",
		"role.scope", "role.undeny", "role.update", "trigger.secret",
		// agents.free — только master: команда проверяет это сама.
		"agents.free", "ha.contour"},
	// audit — журнал аудита: кто, что и когда пытался выполнить.
	"audit": {"audit"},
	// server — держать общий оркестратор; remote — подключаться к чужому.
	// Ни то ни другое не входит в потолок llm: нейросеть, подключившись
	// к серверу, где у master нет пароля, стала бы master.
	// ha.init и ha.promote — пара серверов: настроить узел и переключить.
	"server": {"ha.init", "ha.promote", "serve", "tls.init"},
	"remote": {"agent", "connect", "remote"},
}

// Free — команды, доступные любой роли: без них нельзя узнать, кто ты,
// и вернуться обратно. Сравнение точное: role здесь не открывает
// role.create.
var Free = []string{"back.role", "completion", "completion.bash", "completion.fish", "completion.powershell",
	"completion.zsh", "help", "ping", "role", "role.list", "role.proof", "role.show", "set.role", "shell"}

// LLMCeiling — потолок роли llm. Нейросеть предлагает, человек решает
// (П6): выполнять, согласовывать, ставить модули и управлять ролями llm
// не может, даже если master попытается ей это выдать.
var LLMCeiling = []string{"read", "plan", "request"}

var namePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)

// Role — роль и её права.
type Role struct {
	Name    string
	Perms   []string
	Builtin bool
	// Login и Password — хеши; nil — не заданы, и роль доступна без них.
	Login    *Secret
	Password *Secret

	// Deny — запреты. Они перекрывают права, в том числе all: роли с
	// all и запретом serve доступно всё, кроме serve.
	Deny []string
	// Paths — область роли: каталоги, вне которых она не работает.
	// Пусто — области нет.
	Paths []string
	// Limits — пределы частоты действий.
	Limits []Limit
	// TempPerms и TempDeny — права и запреты со сроком (temporary.go).
	TempPerms []TempPerm
	TempDeny  []TempPerm

	// ceiling — потолок системной политики (system.go). Поле не
	// сохраняется: потолок ставится при каждом запуске из файла, которым
	// владеет root, а не из хранилища в рабочем каталоге.
	ceiling *Ceiling
}

// NeedsAuth сообщает, что для входа в роль нужен логин или пароль.
func (r *Role) NeedsAuth() bool { return r.Login != nil || r.Password != nil }

// Verify проверяет логин и пароль. Обе проверки выполняются всегда:
// по времени ответа не узнать, что именно не совпало.
func (r *Role) Verify(login, password string) bool {
	okLogin := r.Login == nil || r.Login.Matches(login)
	okPassword := r.Password == nil || r.Password.Matches(password)
	return okLogin && okPassword
}

// Allows сообщает, может ли роль выполнить команду. command — путь
// команды через точку: diff, fp.check, role.create.
func (r *Role) Allows(command string) bool {
	if contains(Free, command) {
		return true
	}
	if r.Denies(command) {
		return false
	}
	if r.ceiling != nil && !r.ceiling.allows(command) {
		return false
	}
	for _, perm := range append(append([]string(nil), r.Perms...), active(r.TempPerms, Now())...) {
		if Covers(perm, command) {
			return true
		}
	}
	return false
}

// Denies сообщает, что команда роли запрещена.
func (r *Role) Denies(command string) bool {
	if contains(Free, command) {
		return false
	}
	for _, perm := range append(append([]string(nil), r.Deny...), active(r.TempDeny, Now())...) {
		if blocks(perm, command) {
			return true
		}
	}
	return false
}

// blocks — запрет закрывает команду. Запрет шире права: запрет команды
// верхнего уровня закрывает все её подкоманды, даже со своей группой.
func blocks(perm, command string) bool {
	top := command
	if i := strings.IndexByte(command, '.'); i > 0 {
		top = command[:i]
	}
	if perm == All || perm == command || perm == top {
		return true
	}
	for _, member := range Groups[perm] {
		if member == command || member == top {
			return true
		}
	}
	return false
}

// Covers сообщает, что право открывает команду: это all, сама команда,
// команда верхнего уровня (fp открывает fp.check) или группа, в которой
// есть одно из них. Вложенная команда, у которой своя группа, открывается
// только ею: право inventory (чтение) не открывает inventory.ack (решение).
func Covers(perm, command string) bool {
	top := command
	if i := strings.IndexByte(command, '.'); i > 0 {
		top = command[:i]
	}
	own := top != command && grouped(command)
	if perm == All || perm == command || (perm == top && !own) {
		return true
	}
	for _, member := range Groups[perm] {
		if member == command || (member == top && !own) {
			return true
		}
	}
	return false
}

// grouped — команда сама названа в какой-нибудь группе.
func grouped(command string) bool {
	for _, members := range Groups {
		if contains(members, command) {
			return true
		}
	}
	return false
}

// CheckPerms проверяет, что права существуют: опечатка в праве иначе
// выдала бы роли пустое место. extra — команды, которых нет в группах,
// например команды подключённых модулей.
func CheckPerms(perms, extra []string) error {
	if len(perms) == 0 {
		return errors.New("не указано ни одного права")
	}
	known := append(KnownCommands(), extra...)
	for _, perm := range perms {
		perm = Canonical(perm)
		if perm == All {
			continue
		}
		if _, group := Groups[perm]; group {
			continue
		}
		if !contains(known, perm) {
			return fmt.Errorf("неизвестное право %q: группы %s, all или имя команды", perm, strings.Join(GroupNames(), ", "))
		}
	}
	return nil
}

// KnownCommands — команды, у которых есть группа.
func KnownCommands() []string {
	var commands []string
	for _, members := range Groups {
		commands = append(commands, members...)
	}
	sort.Strings(commands)
	return commands
}

// GroupNames — имена групп по порядку.
func GroupNames() []string {
	names := make([]string, 0, len(Groups))
	for name := range Groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// withinCeiling сообщает, укладывается ли право в потолок llm.
func withinCeiling(perm string) bool {
	if contains(LLMCeiling, perm) {
		return true
	}
	for _, group := range LLMCeiling {
		if contains(Groups[group], perm) {
			return true
		}
	}
	return false
}

func checkName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("имя роли %q: латинская буква, затем буквы, цифры, _ или -, до 32 символов", name)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
