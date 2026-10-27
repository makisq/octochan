package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"octochan/i18n"
)

// DefaultPrefix — префикс архивного журнала по умолчанию: дата и время
// переноса.
const DefaultPrefix = "%Y-%m-%d_%H-%M-%S_"

// Rotator — журнал, который по достижении размера уходит в архив.
//
// Пишется всегда один файл — Dir/Name, и ochan logs читает его. Когда
// очередная запись не помещается в MaxSize, файл переименовывается в
// архивный — с префиксом по шаблону Prefix, по умолчанию дата и время, —
// и запись идёт в новый. Архивов хранится не больше Keep.
type Rotator struct {
	Dir  string
	Name string
	// MaxSize — предел размера в байтах; 0 — журнал не переносится.
	MaxSize int64
	// Prefix — шаблон префикса архива: %Y %m %d %H %M %S и %%.
	Prefix string
	// Keep — сколько архивов хранить; 0 — все.
	Keep int
	// Now — часы; nil — time.Now. Нужны тестам.
	Now func() time.Time

	mu   sync.Mutex
	file *os.File
	size int64
}

// CheckPrefix проверяет шаблон префикса: архив должен лечь в тот же
// каталог, поэтому разделителей пути в префиксе быть не может.
func CheckPrefix(pattern string) error {
	expanded := FormatPrefix(pattern, time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC))
	if strings.ContainsAny(expanded, `/\`) || strings.Contains(expanded, "..") {
		return i18n.Errorf("logs.prefix %q: в префиксе не может быть / \\ или ..", pattern)
	}
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '%' {
			continue
		}
		if i+1 >= len(pattern) || !strings.ContainsRune("YmdHMS%", rune(pattern[i+1])) {
			return i18n.Errorf("logs.prefix %q: допустимы %%Y %%m %%d %%H %%M %%S и %%%%", pattern)
		}
		i++
	}
	return nil
}

// FormatPrefix подставляет время в шаблон префикса.
func FormatPrefix(pattern string, t time.Time) string {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c != '%' || i+1 >= len(pattern) {
			b.WriteByte(c)
			continue
		}
		i++
		switch pattern[i] {
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.Year())
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(pattern[i])
		}
	}
	return b.String()
}

var sizePattern = regexp.MustCompile(`(?i)^\s*(\d+)\s*(b|kb|mb|gb|кб|мб|гб|k|m|g)?\s*$`)

// ParseSize разбирает размер: 100MB, 512KB, 1GB. Число без единицы —
// мегабайты, как в прежнем defaults.log_max_size.
func ParseSize(text string) (int64, error) {
	m := sizePattern.FindStringSubmatch(text)
	if m == nil {
		return 0, i18n.Errorf("размер %q: ожидается число с единицей, например 100MB", text)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, i18n.Errorf("размер %q: %v", text, err)
	}
	unit := int64(1 << 20)
	switch strings.ToLower(m[2]) {
	case "b":
		unit = 1
	case "kb", "кб", "k":
		unit = 1 << 10
	case "gb", "гб", "g":
		unit = 1 << 30
	}
	if n > (1<<62)/unit {
		return 0, i18n.Errorf("размер %q слишком велик", text)
	}
	return n * unit, nil
}

func (r *Rotator) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Rotator) path() string { return filepath.Join(r.Dir, r.Name) }

// Write пишет в журнал, перенося его в архив, когда запись не помещается.
func (r *Rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.open(); err != nil {
		return 0, err
	}
	if r.MaxSize > 0 && r.size > 0 && r.size+int64(len(p)) > r.MaxSize {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

// open открывает журнал. Если файл на месте уже не тот — его перенёс в
// архив другой процесс ochan, — открывается новый: иначе запись ушла бы
// в архив.
func (r *Rotator) open() error {
	if r.file != nil {
		onDisk, err := os.Stat(r.path())
		mine, mineErr := r.file.Stat()
		if err == nil && mineErr == nil && os.SameFile(onDisk, mine) {
			r.size = mine.Size()
			return nil
		}
		r.file.Close()
		r.file = nil
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(r.path(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.file, r.size = f, info.Size()
	return nil
}

// rotate переносит журнал в архив и открывает новый.
func (r *Rotator) rotate() error {
	r.file.Close()
	r.file = nil

	prefix := FormatPrefix(r.prefix(), r.now())
	target := filepath.Join(r.Dir, prefix+r.Name)
	// Два переноса в одну секунду (или префикс без времени) не должны
	// затирать друг друга.
	for n := 1; ; n++ {
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			break
		}
		target = filepath.Join(r.Dir, prefix+strconv.Itoa(n)+"."+r.Name)
	}
	if err := os.Rename(r.path(), target); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := r.prune(); err != nil {
		return err
	}
	return r.open()
}

func (r *Rotator) prefix() string {
	if r.Prefix == "" {
		return DefaultPrefix
	}
	return r.Prefix
}

// Archive — архивный журнал.
type Archive struct {
	Path     string
	Size     int64
	Modified time.Time

	// stamp и seq — время из префикса и номер переноса в этот момент:
	// по ним архивы упорядочиваются (см. Archives).
	stamp time.Time
	seq   int
}

// archiveMatcher узнаёт архив по имени: префикс по шаблону, номер при
// совпадении и имя журнала. Архив узнаётся по шаблону, а не по окончанию
// имени: журнал может лежать в общем каталоге, и чужой file.octochan.log
// удалять нельзя.
type archiveMatcher struct {
	re *regexp.Regexp
	// fields — поля времени (Y, m, d, H, M, S) в порядке групп выражения.
	fields []byte
}

func newArchiveMatcher(prefix, name string) archiveMatcher {
	var b strings.Builder
	var fields []byte
	b.WriteString("^")
	for i := 0; i < len(prefix); i++ {
		if prefix[i] == '%' && i+1 < len(prefix) {
			i++
			switch prefix[i] {
			case 'Y':
				b.WriteString(`(\d{4})`)
				fields = append(fields, 'Y')
			case 'm', 'd', 'H', 'M', 'S':
				b.WriteString(`(\d{2})`)
				fields = append(fields, prefix[i])
			default:
				b.WriteString(regexp.QuoteMeta(string(prefix[i])))
			}
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(prefix[i])))
	}
	b.WriteString(`(?:(\d+)\.)?`)
	b.WriteString(regexp.QuoteMeta(name))
	b.WriteString("$")
	return archiveMatcher{re: regexp.MustCompile(b.String()), fields: fields}
}

// match разбирает имя файла: время из префикса и номер переноса.
// ok == false — это не архив журнала.
func (m archiveMatcher) match(file string) (stamp time.Time, seq int, ok bool) {
	sub := m.re.FindStringSubmatch(file)
	if sub == nil {
		return time.Time{}, 0, false
	}
	// Поля, которых в префиксе нет, — начало соответствующего периода.
	value := map[byte]int{'Y': 1, 'm': 1, 'd': 1}
	for i, field := range m.fields {
		value[field], _ = strconv.Atoi(sub[i+1])
	}
	if s := sub[len(m.fields)+1]; s != "" {
		seq, _ = strconv.Atoi(s)
	}
	stamp = time.Date(value['Y'], time.Month(value['m']), value['d'],
		value['H'], value['M'], value['S'], 0, time.UTC)
	return stamp, seq, true
}

// Archives возвращает журнал и его архивы с префиксом по шаблону prefix
// (пустой — DefaultPrefix), новые первыми.
func Archives(dir, name, prefix string) ([]Archive, error) {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	matcher := newArchiveMatcher(prefix, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Archive
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		stamp, seq, ok := matcher.match(e.Name())
		if e.Name() != name && !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Archive{Path: filepath.Join(dir, e.Name()), Size: info.Size(),
			Modified: info.ModTime(), stamp: stamp, seq: seq})
	}

	// Порядок — по тому, что записано в имени: время из префикса, затем
	// номер переноса. Время изменения файла — только последний довод: на
	// Linux его отметка грубее миллисекунды, и переносы подряд получали
	// одинаковое время. Тогда решало сравнение имён, а по нему
	// «2026-10-23_octochan.log» новее «2026-10-23_1.octochan.log» —
	// и удалялся не самый старый архив, а более новый.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// Текущий журнал — всегда первым: он новее любого архива.
		if current, other := filepath.Base(a.Path) == name, filepath.Base(b.Path) == name; current != other {
			return current
		}
		if !a.stamp.Equal(b.stamp) {
			return a.stamp.After(b.stamp)
		}
		if a.seq != b.seq {
			return a.seq > b.seq
		}
		if !a.Modified.Equal(b.Modified) {
			return a.Modified.After(b.Modified)
		}
		return a.Path > b.Path
	})
	return out, nil
}

// prune удаляет архивы сверх Keep, начиная со старых.
func (r *Rotator) prune() error {
	if r.Keep <= 0 {
		return nil
	}
	all, err := Archives(r.Dir, r.Name, r.prefix())
	if err != nil {
		return err
	}
	kept := 0
	for _, a := range all {
		if filepath.Base(a.Path) == r.Name {
			continue
		}
		kept++
		if kept > r.Keep {
			if err := os.Remove(a.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// Close закрывает журнал.
func (r *Rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
