package lang

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"octochan/config"
)

// EffectKind — вид действия сценария, видимого за пределами интерпретатора.
type EffectKind string

const (
	// EffectWrite — write(): файл записан произвольным текстом.
	EffectWrite EffectKind = "write"
	// EffectSave — save(): в файл записан конфиг.
	EffectSave EffectKind = "save"
	// EffectLive — прочитан живой источник. Ничего не меняет, но план
	// обязан об этом сказать: это обращение к боевому серверу.
	EffectLive EffectKind = "live"
)

// Effect — одно действие сценария.
type Effect struct {
	Kind EffectKind
	// Path — реальный путь файла или имя живого источника (без пароля).
	Path string
	// Existed сообщает, что файл существовал до записи; Before — его
	// прежнее содержимое.
	Existed bool
	Before  []byte
	// After — записанное содержимое.
	After []byte
}

// Fingerprint — отпечаток действия. По отпечаткам согласованный план
// сверяется с тем, что сценарий сделал бы сейчас.
func (e Effect) Fingerprint() string {
	if e.Kind == EffectLive {
		return string(e.Kind) + " " + e.Path
	}
	return fmt.Sprintf("%s %s %x", e.Kind, e.Path, sha256.Sum256(e.After))
}

// overlay — файлы, записанные в режиме плана.
//
// На диск ничего не попадает, но чтение того же файла дальше по сценарию
// должно видеть записанное, иначе план описывал бы не тот сценарий, что
// выполнится. Он общий для всех итераций parallel for, как и диск.
type overlay struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (o *overlay) get(path string) ([]byte, bool) {
	if o == nil {
		return nil, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.files[path]
	return data, ok
}

func (o *overlay) put(path string, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.files[path] = data
}

// SetSandbox ограничивает встроенные функции песочницей.
func (in *Interpreter) SetSandbox(box *Sandbox) { in.sandbox = box }

// SetDir задаёт каталог, от которого отсчитываются относительные пути.
// Пустая строка — текущий каталог процесса.
func (in *Interpreter) SetDir(dir string) { in.dir = dir }

// SetPlanMode включает режим плана: write и save ничего не записывают,
// а только попадают в список действий. Чтение файлов и живых источников
// выполняется по-настоящему: без него сценарию нечего было бы планировать.
func (in *Interpreter) SetPlanMode() { in.dry = &overlay{files: make(map[string][]byte)} }

// Planning сообщает, что интерпретатор работает в режиме плана.
func (in *Interpreter) Planning() bool { return in.dry != nil }

// Effects возвращает действия сценария в порядке выполнения. Действия
// итераций parallel for идут в порядке элементов, а не завершения.
func (in *Interpreter) Effects() []Effect { return append([]Effect(nil), in.effects...) }

// path приводит путь из сценария к абсолютному и проверяет его песочницей.
func (in *Interpreter) path(name string) (string, error) {
	if name == "" {
		return "", errors.New("пустой путь")
	}

	path := name
	if !filepath.IsAbs(path) {
		base := in.dir
		if base == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return "", fmt.Errorf("не удалось определить текущий каталог: %w", err)
			}
			base = cwd
		}
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)

	if in.sandbox != nil {
		return in.sandbox.resolve(path)
	}
	return path, nil
}

// readFile читает файл: сначала записанное в режиме плана, затем диск.
func (in *Interpreter) readFile(name string) ([]byte, error) {
	path, err := in.path(name)
	if err != nil {
		return nil, err
	}
	if data, ok := in.dry.get(path); ok {
		return data, nil
	}

	// Каталог, устройство или канал на месте файла — либо ошибка, либо
	// зависание при чтении. В песочнице читаются только обычные файлы.
	if in.sandbox != nil {
		if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s — не обычный файл", name)
		}
	}
	return os.ReadFile(path)
}

// fileExists сообщает, существует ли файл, с учётом режима плана.
func (in *Interpreter) fileExists(name string) (bool, error) {
	path, err := in.path(name)
	if err != nil {
		return false, err
	}
	if _, ok := in.dry.get(path); ok {
		return true, nil
	}
	return config.FileExists(path), nil
}

// writeFile записывает файл или, в режиме плана, только запоминает запись.
func (in *Interpreter) writeFile(kind EffectKind, name string, data []byte) error {
	path, err := in.path(name)
	if err != nil {
		return err
	}

	effect := Effect{Kind: kind, Path: path, After: data}
	if before, ok := in.dry.get(path); ok {
		effect.Existed, effect.Before = true, before
	} else if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s — не обычный файл", name)
		}
		effect.Existed = true
		effect.Before, _ = os.ReadFile(path)
	}

	if in.dry != nil {
		// План не должен обещать то, что не выполнится: запись в
		// несуществующий каталог упадёт и при настоящем запуске.
		if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
			return fmt.Errorf("каталог для %s не существует", name)
		}
		in.dry.put(path, data)
	} else if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}

	in.effects = append(in.effects, effect)
	return nil
}

// loadLive читает живой источник, если песочница это разрешает.
func (in *Interpreter) loadLive(ref, scheme string, opts config.SourceOptions) (*config.LoadedConfig, error) {
	if in.sandbox != nil {
		if err := in.sandbox.checkLive(scheme); err != nil {
			return nil, err
		}
	}
	loaded, err := config.LoadConfigFrom(in.ctx, ref, opts)
	if err != nil {
		return nil, err
	}
	in.effects = append(in.effects, Effect{Kind: EffectLive, Path: loaded.Name})
	return loaded, nil
}

// lookupEnv читает переменную окружения, если песочница это разрешает.
func (in *Interpreter) lookupEnv(name string) (string, bool, error) {
	if in.sandbox != nil {
		if err := in.sandbox.checkEnv(name); err != nil {
			return "", false, err
		}
	}
	value, ok := os.LookupEnv(name)
	return value, ok, nil
}

// BuildPlan описывает действия сценария как план — тот же, что у patch
// и у сценариев исполнителей, чтобы его можно было положить в заявку.
//
// Порядок изменений — порядок выполнения, а не алфавит: в сценарии
// вторая запись того же файла перекрывает первую, и это должно быть видно.
func BuildPlan(source string, effects []Effect) *config.Plan {
	plan := &config.Plan{Command: "run", Source: source}

	seenLive := make(map[string]bool)
	for _, e := range effects {
		switch e.Kind {
		case EffectLive:
			if !seenLive[e.Path] {
				seenLive[e.Path] = true
				plan.Warn("сценарий читает живой источник %s", e.Path)
			}
		case EffectWrite:
			planWrite(plan, e)
		case EffectSave:
			planSave(plan, e)
		}
	}
	return plan
}

func planWrite(plan *config.Plan, e Effect) {
	var detail string
	switch {
	case !e.Existed:
		detail = "новый файл, " + byteCount(len(e.After))
	case bytes.Equal(e.Before, e.After):
		return // запись того же содержимого ничего не меняет
	default:
		detail = fmt.Sprintf("перезапись: было %s, станет %s", byteCount(len(e.Before)), byteCount(len(e.After)))
	}
	plan.Add(config.Change{Kind: config.ChangeWrite, Target: e.Path, Name: "файл", Detail: detail})
}

// planSave раскладывает запись конфига по параметрам.
//
// save пишет файл целиком, поэтому параметр, которого нет в новом конфиге,
// из файла исчезнет: в отличие от patch, здесь бывают и удаления.
func planSave(plan *config.Plan, e Effect) {
	after, err := config.ParseConfig(string(e.After))
	if err != nil {
		plan.Add(config.Change{Kind: config.ChangeWrite, Target: e.Path, Name: "файл",
			Detail: "записываемый конфиг не разбирается: " + err.Error()})
		return
	}

	var before config.ConfigMap
	if e.Existed {
		if bytes.Equal(e.Before, e.After) {
			return
		}
		parsed, err := config.ParseConfig(string(e.Before))
		if err != nil {
			plan.Add(config.Change{Kind: config.ChangeWrite, Target: e.Path, Name: "файл",
				Detail: "перезапись файла, который не разбирается как конфиг"})
			return
		}
		before = parsed
		if hasComments(e.Before) {
			plan.Warn("save перепишет %s целиком: комментарии и порядок строк не сохранятся", e.Path)
		}
	}

	diff := config.CompareConfigs(before, after, e.Path, e.Path)
	for _, entry := range diff.Entries {
		kind := config.ChangeSet
		switch {
		case !entry.LeftExists:
			kind = config.ChangeAdd
		case !entry.RightExists:
			kind = config.ChangeRemove
		}
		plan.Add(config.Change{Kind: kind, Target: e.Path, Name: entry.Param(), From: entry.Left, To: entry.Right})
	}

	if diff.Len() == 0 {
		detail := "параметры не меняются, файл будет переписан"
		if !e.Existed {
			detail = "новый файл без параметров"
		}
		plan.Add(config.Change{Kind: config.ChangeWrite, Target: e.Path, Name: "файл", Detail: detail})
	}
}

func hasComments(text []byte) bool {
	for _, line := range strings.Split(string(text), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			return true
		}
	}
	return false
}

func byteCount(n int) string {
	return fmt.Sprintf("%d %s", n, plural(n, "байт", "байта", "байт"))
}
