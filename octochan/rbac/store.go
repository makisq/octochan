package rbac

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileName — хранилище ролей в рабочем каталоге octochan.
const FileName = "roles.bin"

// historyLimit — сколько переключений помнит история.
const historyLimit = 32

// magic открывает файл хранилища: по нему видно, что это не мусор и не
// чужой файл с тем же именем.
var magic = [8]byte{'O', 'C', 'H', 'R', 'O', 'L', 'E', 0}

const formatVersion uint16 = 1

// Store — роли, текущая роль и история переключений.
type Store struct {
	Roles []*Role
	// Current — текущая роль.
	Current string
	// History — роли в порядке переключений; последняя — текущая.
	History []string
	// ServerKey — ключ подписи допусков сервера; пуст, пока сервер не
	// запускался (см. grant.go).
	ServerKey []byte
	// CurrentUntil — срок текущей роли; нулевой — бессрочно. По
	// истечении текущей становится ReturnTo (temporary.go).
	CurrentUntil time.Time
	ReturnTo     string
}

// Default — хранилище сразу после установки: master — первая и
// текущая роль, llm — роль для нейросетей.
func Default() *Store {
	return &Store{
		Roles: []*Role{
			{Name: Master, Perms: []string{All}, Builtin: true},
			// Нейросеть, которой подсунули вредную инструкцию, может
			// завалить людей заявками: тридцати в час хватает для работы.
			{Name: LLM, Perms: append([]string(nil), LLMCeiling...), Builtin: true,
				Limits: []Limit{{Perm: "request", Count: 30, Window: time.Hour}}},
		},
		Current: Master,
		History: []string{Master},
	}
}

// Find ищет роль без учёта регистра: Dan и dan — одна роль, иначе
// back role dan искал бы не ту.
func (s *Store) Find(name string) *Role {
	for _, role := range s.Roles {
		if strings.EqualFold(role.Name, name) {
			return role
		}
	}
	return nil
}

// CurrentRole возвращает текущую роль.
func (s *Store) CurrentRole() *Role { return s.Find(s.Current) }

func (s *Store) role(name string) (*Role, error) {
	role := s.Find(name)
	if role == nil {
		return nil, fmt.Errorf("роли %s нет", name)
	}
	return role, nil
}

// Create добавляет роль с правами.
func (s *Store) Create(name string, perms, extra []string) (*Role, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	if existing := s.Find(name); existing != nil {
		return nil, fmt.Errorf("роль %s уже есть", existing.Name)
	}
	if err := CheckPerms(perms, extra); err != nil {
		return nil, err
	}
	role := &Role{Name: name, Perms: dedupe(perms)}
	s.Roles = append(s.Roles, role)
	return role, nil
}

// Delete удаляет роль. Встроенные и текущую удалить нельзя.
func (s *Store) Delete(name string) error {
	role, err := s.role(name)
	if err != nil {
		return err
	}
	if role.Builtin {
		return fmt.Errorf("роль %s встроенная, её нельзя удалить", role.Name)
	}
	if strings.EqualFold(role.Name, s.Current) {
		return fmt.Errorf("роль %s текущая: сначала переключитесь на другую", role.Name)
	}
	for i, r := range s.Roles {
		if r == role {
			s.Roles = append(s.Roles[:i], s.Roles[i+1:]...)
			break
		}
	}
	s.forget(role.Name, false)
	return nil
}

// Grant выдаёт роли права.
func (s *Store) Grant(name string, perms, extra []string) error {
	role, err := s.role(name)
	if err != nil {
		return err
	}
	if err := checkGrant(role, perms, extra); err != nil {
		return err
	}
	role.Perms = dedupe(append(role.Perms, perms...))
	return nil
}

// checkGrant — можно ли выдать роли права: у master их и так все, а llm
// не поднимается выше потолка ни навсегда, ни на время.
func checkGrant(role *Role, perms, extra []string) error {
	if role.Name == Master {
		return errors.New("у master все права, выдавать нечего")
	}
	if err := CheckPerms(perms, extra); err != nil {
		return err
	}
	if role.Name == LLM {
		for _, perm := range perms {
			if !withinCeiling(perm) {
				return fmt.Errorf("право %s выше потолка роли llm (%s): нейросеть предлагает, решает человек",
					perm, strings.Join(LLMCeiling, ", "))
			}
		}
	}
	return nil
}

// Revoke забирает у роли права.
func (s *Store) Revoke(name string, perms []string) error {
	role, err := s.role(name)
	if err != nil {
		return err
	}
	if role.Name == Master {
		return errors.New("у master права не забираются: это роль, в которой доступно всё")
	}
	var kept []string
	for _, perm := range role.Perms {
		if !contains(perms, perm) {
			kept = append(kept, perm)
		}
	}
	temp := withoutPerms(role.TempPerms, perms)
	if len(kept) == len(role.Perms) && len(temp) == len(role.TempPerms) {
		return fmt.Errorf("у роли %s нет таких прав: %s", role.Name, strings.Join(perms, ", "))
	}
	role.Perms, role.TempPerms = kept, temp
	return nil
}

func withoutPerms(list []TempPerm, perms []string) []TempPerm {
	var out []TempPerm
	for _, t := range list {
		if !contains(perms, t.Perm) {
			out = append(out, t)
		}
	}
	return out
}

// Rename переименовывает роль. История и текущая роль следуют за ней.
func (s *Store) Rename(name, newName string) error {
	role, err := s.role(name)
	if err != nil {
		return err
	}
	if role.Builtin {
		return fmt.Errorf("роль %s встроенная, её нельзя переименовать", role.Name)
	}
	if err := checkName(newName); err != nil {
		return err
	}
	if other := s.Find(newName); other != nil && other != role {
		return fmt.Errorf("роль %s уже есть", other.Name)
	}
	old := role.Name
	role.Name = newName
	if strings.EqualFold(s.Current, old) {
		s.Current = newName
	}
	if strings.EqualFold(s.ReturnTo, old) {
		s.ReturnTo = newName
	}
	for i, entry := range s.History {
		if strings.EqualFold(entry, old) {
			s.History[i] = newName
		}
	}
	return nil
}

// SetLogin задаёт или снимает логин роли; nil — снять.
func (s *Store) SetLogin(name string, login *string) error {
	return s.setSecret(name, login, func(r *Role, sec *Secret) { r.Login = sec })
}

// SetPassword задаёт или снимает пароль роли; nil — снять.
func (s *Store) SetPassword(name string, password *string) error {
	return s.setSecret(name, password, func(r *Role, sec *Secret) { r.Password = sec })
}

func (s *Store) setSecret(name string, value *string, set func(*Role, *Secret)) error {
	role, err := s.role(name)
	if err != nil {
		return err
	}
	var secret *Secret
	if value != nil {
		if *value == "" {
			return errors.New("пустое значение: чтобы снять логин или пароль, используйте снятие, а не пустую строку")
		}
		if secret, err = NewSecret(*value); err != nil {
			return err
		}
	}
	set(role, secret)
	// Вход по истории без пароля держится на том, что пароль уже вводили.
	// Новый пароль этого не подтверждён: роль уходит из истории.
	s.forget(role.Name, true)
	return nil
}

// Switch делает роль текущей. Логин и пароль проверяет вызывающий: здесь
// роль уже допущена.
func (s *Store) Switch(name string) error {
	role, err := s.role(name)
	if err != nil {
		return err
	}
	s.Current = role.Name
	s.CurrentUntil, s.ReturnTo = time.Time{}, ""
	s.History = append(s.History, role.Name)
	if len(s.History) > historyLimit {
		s.History = s.History[len(s.History)-historyLimit:]
	}
	return nil
}

// BackTarget выбирает роль для back role.
//
// Без имени — роль, бывшая текущей до последнего переключения. Поэтому
// два back role подряд возвращают туда же, откуда начали: set role slave,
// back role — master, back role — снова slave. С именем — эта роль, если
// она есть в истории: в неё уже входили, и логин с паролем вводить заново
// не нужно.
func (s *Store) BackTarget(name string) (*Role, error) {
	if name == "" {
		for i := len(s.History) - 2; i >= 0; i-- {
			if strings.EqualFold(s.History[i], s.Current) {
				continue
			}
			if role := s.Find(s.History[i]); role != nil {
				return role, nil
			}
		}
		return nil, errors.New("возвращаться некуда: роль ещё не переключалась")
	}

	role, err := s.role(name)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(role.Name, s.Current) {
		return nil, fmt.Errorf("роль %s уже текущая", role.Name)
	}
	for _, entry := range s.History {
		if strings.EqualFold(entry, role.Name) {
			return role, nil
		}
	}
	return nil, fmt.Errorf("роли %s нет в истории переключений: в неё входят через set role %s", role.Name, role.Name)
}

// forget убирает роль из истории. keepCurrent оставляет последнюю
// запись, если роль текущая: история всегда кончается текущей ролью.
func (s *Store) forget(name string, keepCurrent bool) {
	var kept []string
	for i, entry := range s.History {
		last := i == len(s.History)-1
		if strings.EqualFold(entry, name) && !(keepCurrent && last) {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 || !strings.EqualFold(kept[len(kept)-1], s.Current) {
		kept = append(kept, s.Current)
	}
	s.History = kept
}

func dedupe(perms []string) []string {
	var out []string
	for _, perm := range perms {
		if perm = strings.TrimSpace(perm); perm != "" && !contains(out, perm) {
			out = append(out, perm)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Файл
// ---------------------------------------------------------------------------

// Load читает хранилище. Файла нет — это первая установка: роли по
// умолчанию. Испорченный файл — ошибка, а не тихий откат к master: иначе
// порча файла снимала бы все пароли.
func Load(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать хранилище ролей: %w", err)
	}
	store, err := decode(data)
	if err != nil {
		return nil, fmt.Errorf("хранилище ролей %s повреждено: %w; удалить его — значит вернуть роли "+
			"к исходным (master без пароля и llm)", path, err)
	}
	return store, nil
}

// Save записывает хранилище с правами 0600. Запись через временный файл
// и переименование: оборванная запись не оставит полфайла.
func (s *Store) Save(path string) error {
	data, err := s.encode()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".roles-*")
	if err != nil {
		return fmt.Errorf("не удалось записать хранилище ролей: %w", err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return fmt.Errorf("не удалось закрыть доступ к хранилищу ролей: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("не удалось записать хранилище ролей: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("не удалось записать хранилище ролей: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("не удалось записать хранилище ролей: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("не удалось записать хранилище ролей: %w", err)
	}
	return nil
}

// Формат: магия, версия формата, длина данных, данные gob, SHA-256 данных.
// Сумма ловит порчу файла; от владельца компьютера она не защищает и не
// должна: локально он и так master.
func (s *Store) encode() ([]byte, error) {
	var payload bytes.Buffer
	if err := gob.NewEncoder(&payload).Encode(s); err != nil {
		return nil, fmt.Errorf("не удалось закодировать роли: %w", err)
	}

	var out bytes.Buffer
	out.Write(magic[:])
	_ = binary.Write(&out, binary.BigEndian, formatVersion)
	_ = binary.Write(&out, binary.BigEndian, uint32(payload.Len()))
	out.Write(payload.Bytes())
	sum := sha256.Sum256(payload.Bytes())
	out.Write(sum[:])
	return out.Bytes(), nil
}

func decode(data []byte) (*Store, error) {
	const header = len(magic) + 2 + 4
	if len(data) < header+sha256.Size || !bytes.Equal(data[:len(magic)], magic[:]) {
		return nil, errors.New("это не файл ролей octochan")
	}
	version := binary.BigEndian.Uint16(data[len(magic):])
	if version != formatVersion {
		return nil, fmt.Errorf("формат версии %d, эта сборка понимает %d", version, formatVersion)
	}
	size := int(binary.BigEndian.Uint32(data[len(magic)+2:]))
	if size != len(data)-header-sha256.Size {
		return nil, errors.New("длина данных не сходится")
	}

	payload := data[header : header+size]
	sum := sha256.Sum256(payload)
	if !bytes.Equal(sum[:], data[header+size:]) {
		return nil, errors.New("контрольная сумма не сходится")
	}

	var store Store
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&store); err != nil {
		return nil, fmt.Errorf("данные не читаются: %w", err)
	}
	if store.Find(Master) == nil || store.Find(LLM) == nil || store.CurrentRole() == nil {
		return nil, errors.New("нет встроенных ролей или текущей роли")
	}
	if len(store.History) == 0 {
		store.History = []string{store.Current}
	}
	return &store, nil
}
