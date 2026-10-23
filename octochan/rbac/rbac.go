// Package rbac — роли octochan и их права.
//
// После установки есть две роли. master — первая и высшая, ей доступно
// всё; на своём компьютере ты master. llm — роль для нейросетей: читать,
// планировать и оформлять заявки, но не выполнять и не согласовывать.
// Остальные роли создаёт master и выдаёт им права.
//
// Локально роли — защита от ошибки и граница для нейросети, а не от
// владельца компьютера: кто может удалить ~/.octochan, тот вернёт роли
// к исходным. Границей против чужих они станут на общем сервере, где
// хранилище лежит у сервера, а не у того, кто подключается.
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
	"read": {"aliases", "check", "diff", "else", "find", "fp", "history", "if", "lang", "logs",
		"modules", "print", "requests", "services", "stats", "status", "validate"},
	"plan":    {"plan"},
	"request": {"request"},
	"approve": {"approve", "reject"},
	"apply":   {"apply", "eval", "patch", "rollback", "run", "srs", "tee"},
	"install": {"!install", "inspect"},
	"auth":    {"auth", "timer"},
	"admin":   {"role.create", "role.delete", "role.grant", "role.revoke", "role.update"},
	// server — держать общий оркестратор; remote — подключаться к чужому.
	// Ни то ни другое не входит в потолок llm: нейросеть, подключившись
	// к серверу, где у master нет пароля, стала бы master.
	"server": {"serve", "tls.init"},
	"remote": {"connect", "remote"},
}

// Free — команды, доступные любой роли: без них нельзя узнать, кто ты,
// и вернуться обратно. Сравнение точное: role здесь не открывает
// role.create.
var Free = []string{"back.role", "completion", "completion.bash", "completion.fish", "completion.powershell",
	"completion.zsh", "help", "role", "role.list", "set.role", "shell"}

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
	top := command
	if i := strings.IndexByte(command, '.'); i > 0 {
		top = command[:i]
	}

	for _, perm := range r.Perms {
		if perm == All || perm == command || perm == top {
			return true
		}
		for _, member := range Groups[perm] {
			if member == command || member == top {
				return true
			}
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
