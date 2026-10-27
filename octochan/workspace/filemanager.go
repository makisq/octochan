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

// FileManager владеет рабочими директориями octochan в домашнем каталоге.
type FileManager struct {
	baseDir      string
	scenariosDir string
	modulesDir   string
	logsDir      string
	reportsDir   string
	snapshotsDir string
	aliasesDir   string
	historyDir   string
	requestsDir  string
	decisionsDir string
}

// NewFileManager создаёт (при необходимости) и возвращает рабочие директории.
func NewFileManager() (*FileManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("не удалось определить домашнюю директорию: %w", err)
	}

	base := filepath.Join(home, ".octochan")
	fm := &FileManager{
		baseDir:      base,
		scenariosDir: filepath.Join(base, "scenarios"),
		modulesDir:   filepath.Join(base, "modules"),
		logsDir:      filepath.Join(base, "logs"),
		reportsDir:   filepath.Join(base, "reports"),
		snapshotsDir: filepath.Join(base, "snapshots"),
		aliasesDir:   filepath.Join(base, "aliases"),
		historyDir:   filepath.Join(base, "history"),
		requestsDir:  filepath.Join(base, "requests"),
		decisionsDir: filepath.Join(base, "decisions"),
	}

	for _, dir := range []string{fm.scenariosDir, fm.modulesDir, fm.logsDir, fm.reportsDir, fm.snapshotsDir, fm.aliasesDir, fm.historyDir, fm.requestsDir, fm.decisionsDir} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("не удалось создать директорию %s: %w", dir, err)
		}
	}

	return fm, nil
}

func (fm *FileManager) BaseDir() string      { return fm.baseDir }
func (fm *FileManager) ScenariosDir() string { return fm.scenariosDir }
func (fm *FileManager) ModulesDir() string   { return fm.modulesDir }
func (fm *FileManager) LogsDir() string      { return fm.logsDir }
func (fm *FileManager) ReportsDir() string   { return fm.reportsDir }
func (fm *FileManager) SnapshotsDir() string { return fm.snapshotsDir }

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
	return filepath.Join(fm.modulesDir, safeName), nil
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
	entries, err := os.ReadDir(fm.modulesDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".stale-") {
			_ = os.Remove(filepath.Join(fm.modulesDir, entry.Name()))
		}
	}
}
