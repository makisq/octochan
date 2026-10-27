package rbac

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Системная политика ролей.
//
// Хранилище ролей лежит в рабочем каталоге пользователя, и пользователь
// может его переписать: локально роли — граница от ошибки, а не от
// владельца. Системная политика отвечает на вопрос «что машина доказывает,
// что пользователь — это роль X?» без рабочего каталога:
//
//   - кто ты — сообщает ядро: uid процесса, имя — из базы пользователей
//     ОС. Переменные окружения не участвуют;
//   - кем тебе можно быть — файл, которым владеет root и в который
//     никто, кроме root, не пишет, как и в каталоги до корня. Его путь
//     задаётся при сборке, а не окружением: окружение — в руках того, кого
//     политика ограничивает.
//
// Политика называет для пользователя ОС роли и потолок прав каждой.
// Хранилище в рабочем каталоге может права только сузить: роль, которой
// нет в политике для этого пользователя, не действует, а права сверх
// потолка — тоже. Чего политика не даёт: она ограничивает ochan, а не
// пользователя. Что пользователь может сделать сам, без ochan, — своими
// файлами и своей сетью, — он сделает и так; граница держит ресурсы,
// до которых он сам не дотягивается (security.md).

// SystemPolicyPath — системная политика ролей. Меняется при сборке:
// -ldflags "-X octochan/rbac.SystemPolicyPath=/opt/octochan/roles.yaml".
var SystemPolicyPath = "/etc/octochan/roles.yaml"

// SystemCheck проверяет, что файлу политики можно верить: владелец root,
// писать в него и в каталоги до корня может только root. Подменяется в
// тестах: в каталоге теста этого не добиться.
var SystemCheck = checkSystemFile

// Ceiling — потолок прав роли по системной политике.
type Ceiling struct {
	Allow []string
	Deny  []string
	// Source — откуда потолок: путь файла политики.
	Source string
}

func (c *Ceiling) allows(command string) bool {
	for _, perm := range c.Deny {
		if blocks(perm, command) {
			return false
		}
	}
	for _, perm := range c.Allow {
		if Covers(perm, command) {
			return true
		}
	}
	return false
}

// Ceiling — потолок, под которым роль действует; nil — потолка нет.
func (r *Role) Ceiling() *Ceiling { return r.ceiling }

// Identity — кто запустил процесс, по словам ядра.
type Identity struct {
	UID  int
	User string // имя из базы пользователей ОС; пусто — не нашлось
}

func (id Identity) String() string {
	if id.User == "" {
		return fmt.Sprintf("uid %d", id.UID)
	}
	return fmt.Sprintf("%s (uid %d)", id.User, id.UID)
}

// SystemPolicy — разобранный файл системной политики.
type SystemPolicy struct {
	Path  string
	Users map[string][]string
	Roles map[string]Ceiling
}

type systemFile struct {
	Users map[string][]string `yaml:"users"`
	Roles map[string]struct {
		Allow []string `yaml:"allow"`
		Deny  []string `yaml:"deny"`
	} `yaml:"roles"`
}

// LoadSystem читает системную политику. Файла нет — nil без ошибки:
// политики нет, роли — как раньше. Файл есть, но верить ему нельзя или он
// испорчен, — ошибка: без обещанной границы ochan не работает. extra —
// команды модулей, которые можно назвать правом.
func LoadSystem(path string, extra []string) (*SystemPolicy, error) {
	if path == "" {
		return nil, nil
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("системная политика ролей %s: %w", path, err)
	}
	if err := SystemCheck(real); err != nil {
		return nil, fmt.Errorf("системной политике ролей %s верить нельзя: %w", path, err)
	}
	// Читается проверенный путь, а не исходный: ссылку по дороге могли
	// подменить, а каталоги проверенного пути не меняет никто, кроме root.
	data, err := os.ReadFile(real)
	if err != nil {
		return nil, fmt.Errorf("системная политика ролей %s: %w", path, err)
	}
	return ParseSystem(path, data, extra)
}

// ParseSystem разбирает файл системной политики.
func ParseSystem(path string, data []byte, extra []string) (*SystemPolicy, error) {
	var f systemFile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("системная политика ролей %s: %w", path, err)
	}
	fail := func(format string, args ...any) (*SystemPolicy, error) {
		return nil, fmt.Errorf("системная политика ролей %s: %s", path, fmt.Sprintf(format, args...))
	}
	if len(f.Users) == 0 {
		return fail("нет users: кому какие роли положены")
	}
	p := &SystemPolicy{Path: path, Users: f.Users, Roles: map[string]Ceiling{}}
	for name, r := range f.Roles {
		if strings.EqualFold(name, Master) {
			return fail("роль master не ограничивается: ей доступно всё; не называйте её пользователю, кому не положено")
		}
		if err := checkName(name); err != nil {
			return fail("%v", err)
		}
		if err := CheckPerms(r.Allow, extra); err != nil {
			return fail("roles.%s.allow: %v", name, err)
		}
		if len(r.Deny) > 0 {
			if err := CheckPerms(r.Deny, extra); err != nil {
				return fail("roles.%s.deny: %v", name, err)
			}
		}
		allow := r.Allow
		if strings.EqualFold(name, LLM) {
			// Потолок llm не выше встроенного, что бы ни написал файл.
			allow = intersectLLM(allow)
		}
		p.Roles[name] = Ceiling{Allow: allow, Deny: r.Deny, Source: path}
	}
	for user, roles := range f.Users {
		if len(roles) == 0 {
			return fail("users.%s: пустой список ролей — уберите пользователя", user)
		}
		if rest, ok := strings.CutPrefix(user, "uid:"); ok {
			if _, err := strconv.Atoi(rest); err != nil {
				return fail("users.%s: после uid: — число", user)
			}
		}
		for _, role := range roles {
			if _, ok := p.Roles[role]; !ok && role != Master {
				return fail("users.%s: роли %s нет в roles — у каждой роли, кроме master, потолок", user, role)
			}
		}
	}
	return p, nil
}

func intersectLLM(allow []string) []string {
	var out []string
	for _, perm := range allow {
		if withinCeiling(perm) {
			out = append(out, perm)
		}
	}
	return out
}

// Allowed — роли, положенные пользователю: по имени, по uid:N или, если
// его не назвали, по «*». Первая — роль по умолчанию.
func (p *SystemPolicy) Allowed(id Identity) []string {
	if id.User != "" {
		if roles, ok := p.Users[id.User]; ok {
			return roles
		}
	}
	if roles, ok := p.Users["uid:"+strconv.Itoa(id.UID)]; ok {
		return roles
	}
	return p.Users["*"]
}

// Permit — можно ли пользователю быть этой ролью.
func (p *SystemPolicy) Permit(id Identity, role string) error {
	allowed := p.Allowed(id)
	if len(allowed) == 0 {
		return fmt.Errorf("пользователь %s не назван в системной политике ролей %s: ролей ochan ему не положено",
			id, p.Path)
	}
	for _, name := range allowed {
		if strings.EqualFold(name, role) {
			return nil
		}
	}
	return fmt.Errorf("роль %s пользователю %s не положена системной политикой %s: положены %s",
		role, id, p.Path, strings.Join(allowed, ", "))
}

// Permits сообщает, что какая-то из положенных пользователю ролей может
// выполнить команду: например, держать сервер (serve).
func (p *SystemPolicy) Permits(id Identity, command string) bool {
	for _, name := range p.Allowed(id) {
		if name == Master {
			return true
		}
		if c, ok := p.Roles[name]; ok && c.allows(command) {
			return true
		}
	}
	return false
}

// Ceil ставит роли потолок политики. master потолка не имеет; роль,
// которой в политике нет, не может ничего, кроме общих команд.
func (p *SystemPolicy) Ceil(role *Role) *Role {
	if role.Name == Master {
		return role
	}
	copied := *role
	c, ok := p.Roles[role.Name]
	if !ok {
		c = Ceiling{Source: p.Path}
	}
	copied.ceiling = &c
	return &copied
}

// Materialize — роль из политики, которой нет в хранилище: права —
// её потолок.
func (p *SystemPolicy) Materialize(name string) *Role {
	c, ok := p.Roles[name]
	if !ok {
		return nil
	}
	return &Role{Name: name, Perms: append([]string(nil), c.Allow...), Deny: append([]string(nil), c.Deny...)}
}

// RoleNames — роли политики по алфавиту.
func (p *SystemPolicy) RoleNames() []string {
	names := make([]string, 0, len(p.Roles))
	for name := range p.Roles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
