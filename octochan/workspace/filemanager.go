package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Права на директории намеренно узкие: в modules лежат исполняемые плагины,
// а в logs/scenarios — рабочие данные пользователя.
const (
	dirPerm        os.FileMode = 0o700
	filePerm       os.FileMode = 0o600
	executablePerm os.FileMode = 0o700
)

// EnvHome — рабочий каталог ochan вместо ~/.octochan. В нём всё состояние:
// роли, журнал аудита, заявки, история, решения, политики, алиасы,
// инциденты, почта, токены и конфиг. Это одно целое: его зеркалит резервный
// узел пары и закрывает от сценариев и ролей защита рабочего каталога —
// поэтому переносится он целиком, а не по частям.
const EnvHome = "OCTOCHAN_HOME"

// Dir — каталог узла, который можно вынести из рабочего каталога отдельно:
// в пару он не копируется, а место ему бывает нужно другое. Модулям — раздел,
// где политика хоста разрешает запуск (домашний каталог с noexec отвечает
// плагину operation not permitted), журналам и отчётам — раздел под данные.
type Dir string

// Выносные каталоги. Значение — ключ в config.yaml: paths.<значение>.
const (
	DirModules       Dir = "modules"
	DirLogs          Dir = "logs"
	DirReports       Dir = "reports"
	DirRegistryCache Dir = "registry_cache"
)

// Dirs — выносные каталоги по порядку.
var Dirs = []Dir{DirModules, DirLogs, DirReports, DirRegistryCache}

// Env — переменная окружения каталога: OCTOCHAN_MODULES.
func (d Dir) Env() string { return "OCTOCHAN_" + strings.ToUpper(string(d)) }

// PathEnv — все переменные окружения, которые задают пути ochan. Кто
// запускает ochan в отдельном доме (стенд, замер), очищает их: иначе пути
// из окружения запускающего увели бы ochan из этого дома.
func PathEnv() []string {
	names := []string{EnvHome}
	for _, d := range Dirs {
		names = append(names, d.Env())
	}
	return names
}

// Откуда взят путь.
const (
	SourceDefault = "по умолчанию"
	sourceEnv     = "окружение "
)

// FileManager владеет рабочими директориями octochan.
type FileManager struct {
	baseDir      string
	baseSource   string
	scenariosDir string
	snapshotsDir string
	aliasesDir   string
	historyDir   string
	requestsDir  string
	decisionsDir string
	// dirs — выносные каталоги; sources — откуда взят каждый.
	dirs    map[Dir]string
	sources map[Dir]string
}

// HomeDir — рабочий каталог ochan: EnvHome, иначе ~/.octochan. Ничего не
// создаёт — для тех, кому нужен только путь (модуль, сервер MCP).
func HomeDir() (string, error) {
	base, _, err := homeDir()
	return base, err
}

func homeDir() (base, source string, err error) {
	if base = os.Getenv(EnvHome); base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("не удалось определить домашнюю директорию: %w (рабочий каталог можно задать в %s)", err, EnvHome)
		}
		return filepath.Join(home, ".octochan"), SourceDefault, nil
	}
	if !filepath.IsAbs(base) {
		return "", "", fmt.Errorf("%s=%s: нужен полный путь — относительный зависел бы от каталога, из которого запущен ochan", EnvHome, base)
	}
	return filepath.Clean(base), sourceEnv + EnvHome, nil
}

// NewFileManager определяет рабочий каталог (EnvHome, иначе ~/.octochan) и
// выносные каталоги из окружения и создаёт рабочий каталог. Каталоги из
// config.yaml добавляет Relocate, остальное создаёт Ensure: конфиг лежит в
// рабочем каталоге и читается после.
func NewFileManager() (*FileManager, error) {
	base, source, err := homeDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(base, dirPerm); err != nil {
		return nil, fmt.Errorf("не удалось создать директорию %s: %w", base, err)
	}
	if source != SourceDefault {
		if err := checkPrivate(base); err != nil {
			return nil, fmt.Errorf("%s: %w", EnvHome, err)
		}
	}

	fm := &FileManager{
		baseDir:      base,
		baseSource:   source,
		scenariosDir: filepath.Join(base, "scenarios"),
		snapshotsDir: filepath.Join(base, "snapshots"),
		aliasesDir:   filepath.Join(base, "aliases"),
		historyDir:   filepath.Join(base, "history"),
		requestsDir:  filepath.Join(base, "requests"),
		decisionsDir: filepath.Join(base, "decisions"),
		dirs: map[Dir]string{
			DirModules:       filepath.Join(base, "modules"),
			DirLogs:          filepath.Join(base, "logs"),
			DirReports:       filepath.Join(base, "reports"),
			DirRegistryCache: filepath.Join(base, "registry", "cache"),
		},
		sources: map[Dir]string{},
	}
	for _, d := range Dirs {
		fm.sources[d] = SourceDefault
		if path := os.Getenv(d.Env()); path != "" {
			if err := fm.relocate(d, path, sourceEnv+d.Env()); err != nil {
				return nil, err
			}
		}
	}
	return fm, nil
}

// Relocate выносит каталог по настройке из config.yaml; source — её имя.
// Окружение сильнее конфига: каталог, уже заданный переменной, остаётся.
func (fm *FileManager) Relocate(d Dir, path, source string) error {
	if strings.HasPrefix(fm.sources[d], sourceEnv) {
		return nil
	}
	return fm.relocate(d, path, source)
}

func (fm *FileManager) relocate(d Dir, path, source string) error {
	if _, known := fm.dirs[d]; !known {
		return fmt.Errorf("%s: такого каталога нет", source)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s: %s — нужен полный путь: относительный зависел бы от каталога, из которого запущен ochan", source, path)
	}
	// Тот же путь, что по умолчанию, — не перенос: прежний конфиг называл
	// каталог журналов явно.
	if path = filepath.Clean(path); path != fm.dirs[d] {
		fm.dirs[d], fm.sources[d] = path, source
	}
	return nil
}

// inside сообщает, что path — это dir или лежит в нём.
func inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Ensure создаёт каталоги и проверяет вынесенные: каталог вне рабочего
// должен принадлежать вам или root и быть закрыт для записи остальным — как
// и каждый каталог над ним. Иначе другой пользователь машины подложил бы в
// модули свой плагин, а ochan его запустил бы.
func (fm *FileManager) Ensure() error {
	dirs := []string{fm.scenariosDir, fm.snapshotsDir, fm.aliasesDir, fm.historyDir, fm.requestsDir, fm.decisionsDir}
	for _, d := range Dirs {
		dirs = append(dirs, fm.dirs[d])
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("не удалось создать директорию %s: %w", dir, err)
		}
	}
	for _, d := range Dirs {
		// Каталог внутри рабочего закрыт вместе с ним.
		if fm.sources[d] == SourceDefault || inside(fm.baseDir, fm.dirs[d]) {
			continue
		}
		if err := checkPrivate(fm.dirs[d]); err != nil {
			return fmt.Errorf("%s: %w", fm.sources[d], err)
		}
	}
	return nil
}

// PathInfo — путь ochan для команды paths: что это, где и откуда взято.
type PathInfo struct {
	Dir    Dir
	Path   string
	Source string
}

// Home — рабочий каталог и откуда он взят.
func (fm *FileManager) Home() PathInfo {
	return PathInfo{Path: fm.baseDir, Source: fm.baseSource}
}

// Relocatable — выносные каталоги: где каждый и откуда взят.
func (fm *FileManager) Relocatable() []PathInfo {
	out := make([]PathInfo, 0, len(Dirs))
	for _, d := range Dirs {
		out = append(out, PathInfo{Dir: d, Path: fm.dirs[d], Source: fm.sources[d]})
	}
	return out
}

func (fm *FileManager) BaseDir() string      { return fm.baseDir }
func (fm *FileManager) ScenariosDir() string { return fm.scenariosDir }
func (fm *FileManager) ModulesDir() string   { return fm.dirs[DirModules] }
func (fm *FileManager) LogsDir() string      { return fm.dirs[DirLogs] }
func (fm *FileManager) ReportsDir() string   { return fm.dirs[DirReports] }
func (fm *FileManager) SnapshotsDir() string { return fm.snapshotsDir }

// RegistryCacheDir — куда клонируются git-реестры пакетов.
func (fm *FileManager) RegistryCacheDir() string { return fm.dirs[DirRegistryCache] }

// AliasesDir — директория с пользовательскими алиасами (*.ochan).
func (fm *FileManager) AliasesDir() string { return fm.aliasesDir }

// HistoryDir — журнал выполненных операций.
func (fm *FileManager) HistoryDir() string { return fm.historyDir }

// RequestsDir — очередь заявок на согласование.
func (fm *FileManager) RequestsDir() string { return fm.requestsDir }

// DecisionsDir — журнал решений формального промпта: из него кольца берут
// надёжность правил.
func (fm *FileManager) DecisionsDir() string { return fm.decisionsDir }

// scenarioPath — единственное место, где формируется путь до сценария.
// Раньше SaveScenario писал в scenarios/, LoadScenario читал из корня, а
// команда srs печатала третий вариант пути.
func (fm *FileManager) scenarioPath(name string) string {
	return filepath.Join(fm.scenariosDir, name+".bin")
}

// SaveScenario сохраняет сценарий и возвращает путь до файла.
func (fm *FileManager) SaveScenario(name string, data []byte) (string, error) {
	path := fm.scenarioPath(name)
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return "", fmt.Errorf("не удалось сохранить сценарий: %w", err)
	}
	return path, nil
}

// LoadScenario читает ранее сохранённый сценарий.
func (fm *FileManager) LoadScenario(name string) ([]byte, error) {
	path := fm.scenarioPath(name)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("сценарий %q не найден в %s", name, fm.scenariosDir)
		}
		return nil, fmt.Errorf("ошибка чтения сценария: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("файл сценария %q пуст", name)
	}

	return data, nil
}

// ModulePath возвращает путь, по которому будет установлен модуль с таким именем.
func (fm *FileManager) ModulePath(name string) (string, error) {
	// Имя приходит извне — берём только базовую часть, чтобы "../../.bashrc"
	// не превратился в запись за пределами директории модулей.
	safeName := filepath.Base(filepath.Clean(name))
	if safeName == "." || safeName == ".." || safeName == string(filepath.Separator) {
		return "", fmt.Errorf("недопустимое имя модуля: %q", name)
	}
	return filepath.Join(fm.ModulesDir(), safeName), nil
}

// InstallHashicorpModule кладёт плагин в директорию модулей.
//
// Прямая перезапись на Windows не годится: пока файл отображён как образ
// работающего процесса, открыть его на запись нельзя. Переименовать —
// можно, и этим мы пользуемся: старый файл отодвигается в сторону, новый
// пишется на освободившееся место, а отодвинутый удаляется, когда система
// его отпустит. Один такой хвост не мешает работе и подбирается при
// следующей установке.
func (fm *FileManager) InstallHashicorpModule(name string, data []byte) (string, error) {
	path, err := fm.ModulePath(name)
	if err != nil {
		return "", err
	}

	fm.sweepStaleModules()

	if _, err := os.Stat(path); err == nil {
		stale := fmt.Sprintf("%s.stale-%d", path, time.Now().UnixNano())
		if err := os.Rename(path, stale); err != nil {
			return "", fmt.Errorf("не удалось освободить %s: %w", filepath.Base(path), err)
		}
		// Удаление может не пройти прямо сейчас — файл подберёт следующая
		// установка. Это лучше, чем отказать в установке целиком.
		_ = os.Remove(stale)
	}

	if err := os.WriteFile(path, data, executablePerm); err != nil {
		return "", fmt.Errorf("не удалось установить модуль: %w", err)
	}
	return path, nil
}

// sweepStaleModules убирает файлы, отодвинутые прошлыми установками.
func (fm *FileManager) sweepStaleModules() {
	entries, err := os.ReadDir(fm.ModulesDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".stale-") {
			_ = os.Remove(filepath.Join(fm.ModulesDir(), entry.Name()))
		}
	}
}
