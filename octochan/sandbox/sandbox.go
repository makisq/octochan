// Package sandbox — изоляция процесса средствами операционной системы.
//
// Песочница языка (lang.Sandbox) проверяет пути, окружение и живые
// источники во встроенных функциях. Это проверка внутри того же процесса:
// найдись в ней ошибка — и сценарий дотянется до всего, что может ochan.
// Здесь вторая стена, которую держит ядро, а не ochan: сценарий выполняется
// отдельным процессом, и, даже обойдя проверки языка, он упирается в отказ
// ядра.
//
// На Linux:
//   - Landlock: файлы — только разрешённые каталоги, остальное ядро
//     отвергает;
//   - seccomp: запрещены exec, ptrace, mount, пространства имён, bpf,
//     io_uring и сигналы чужим процессам, а без сети — и socket();
//   - пространства имён: свой пользователь, чтобы права снаружи не
//     действовали, и своя сеть без интерфейсов, если сеть не нужна;
//   - no_new_privs, а у root — сброс всех привилегий.
//
// Ограничения ставятся в два шага. Landlock действует на поток, а у
// процесса Go потоков много с самого старта. Поэтому первый шаг ограничивает
// один поток и делает exec: новый образ процесса рождается из ограниченного
// потока, и ограничены все его потоки. Второй шаг, уже в новом образе,
// ставит seccomp сразу на все потоки.
//
// На других ОС ядро такого без внешних средств не даёт. Сценарий и там
// выполняется отдельным процессом без окружения родителя, но файлы и сеть
// ограничивает только песочница языка. Об этом говорит отчёт: Report.Gaps.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"octochan/i18n"
)

// Policy — до чего дотягивается изолированный процесс.
type Policy struct {
	// Write — каталоги для чтения и записи: каталоги песочницы сценария.
	Write []string `json:"write,omitempty"`
	// Read — каталоги и файлы только для чтения, например библиотека
	// алиасов.
	Read []string `json:"read,omitempty"`
	// Network разрешает исходящие соединения: они нужны живым источникам.
	Network bool `json:"network,omitempty"`
}

// Report — какая изоляция действует.
type Report struct {
	OS string
	// Landlock — версия ABI Landlock; 0 — Landlock нет.
	Landlock int
	// Seccomp — фильтр системных вызовов.
	Seccomp bool
	// UserNS и NetNS — свои пространства имён пользователя и сети.
	UserNS bool
	NetNS  bool
	// Network — политика разрешила сеть.
	Network bool
}

// Gaps — чего не хватает до полной изоляции, по пункту на пробел.
func (r Report) Gaps() []string {
	if r.OS != "linux" {
		return []string{i18n.Sprintf("на %s ядро не ограничивает процесс без внешних средств: "+
			"файлы и сеть держит только песочница языка", r.OS)}
	}
	var gaps []string
	if r.Landlock == 0 {
		gaps = append(gaps, i18n.T("нет Landlock (нужно ядро 5.13+ с landlock в списке lsm): "+
			"файлы ограничивает только песочница языка"))
	}
	if !r.Seccomp {
		gaps = append(gaps, i18n.T("нет seccomp для этой архитектуры: системные вызовы не ограничены"))
	}
	if !r.Network && !r.NetworkClosed() {
		gaps = append(gaps, i18n.T("сеть не закрыта: нет ни seccomp, ни своего сетевого пространства"))
	}
	return gaps
}

// Full сообщает, что действует вся изоляция, какую обещает пакет.
func (r Report) Full() bool { return len(r.Gaps()) == 0 }

// NetworkClosed сообщает, что сеть закрыта ядром.
func (r Report) NetworkClosed() bool {
	return !r.Network && (r.Seccomp || r.NetNS || r.Landlock >= 4)
}

// Lines — отчёт построчно, для людей.
func (r Report) Lines() []string {
	line := func(title, text string) string { return fmt.Sprintf("%-20s%s", i18n.T(title)+":", text) }
	if r.OS != "linux" {
		return []string{
			line("Отдельный процесс", i18n.T("да, без окружения родителя")),
			line("Файлы", i18n.T("только песочница языка")),
			line("Сеть", i18n.T("только песочница языка")),
		}
	}

	files := i18n.T("только песочница языка")
	if r.Landlock > 0 {
		files = i18n.Sprintf("Landlock ABI %d: только разрешённые каталоги", r.Landlock)
	}
	calls := i18n.T("нет")
	if r.Seccomp {
		calls = i18n.T("seccomp: exec, ptrace, mount, bpf, io_uring и другие запрещены")
	}
	user := i18n.T("нет")
	if r.UserNS {
		user = i18n.T("своё пространство имён, привилегии снаружи не действуют")
	}
	network := i18n.T("открыта: разрешены живые источники")
	if !r.Network {
		var how []string
		if r.NetNS {
			how = append(how, i18n.T("своё сетевое пространство"))
		}
		if r.Seccomp {
			how = append(how, i18n.T("socket() запрещён"))
		}
		if r.Landlock >= 4 {
			how = append(how, i18n.T("TCP запрещён Landlock"))
		}
		network = i18n.T("не закрыта")
		if len(how) > 0 {
			network = i18n.Sprintf("закрыта: %s", strings.Join(how, ", "))
		}
	}
	return []string{
		line("Отдельный процесс", i18n.T("да, без окружения родителя, no_new_privs")),
		line("Файлы", files),
		line("Системные вызовы", calls),
		line("Пользователь", user),
		line("Сеть", network),
	}
}

// Probe сообщает, какую изоляцию получит процесс с этой политикой. Свои
// пространства имён выясняются только при запуске: Process.Report.
func Probe(policy Policy) Report {
	report := Report{OS: runtime.GOOS, Network: policy.Network}
	probe(&report)
	return report
}

// Аргументы, по которым процесс узнаёт, что он изолированный. Нужны оба:
// и аргумент, и переменная окружения, — чтобы случайный вызов `ochan
// __ochan-sandbox` не превратился в ребёнка песочницы.
const (
	childArg = "__ochan-sandbox"
	envStage = "OCTOCHAN_SANDBOX_STAGE"
)

// exitFailed — код выхода, если песочницу поставить не удалось.
const exitFailed = 125

// Command — изолированный процесс: этот же исполняемый файл в песочнице.
type Command struct {
	Policy Policy
	// Env — всё окружение ребёнка: родительское не наследуется.
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Process — запущенный изолированный процесс.
type Process struct {
	cmd    *exec.Cmd
	Report Report
}

// Wait ждёт завершения процесса.
func (p *Process) Wait() error { return p.cmd.Wait() }

// Start запускает процесс. Исполняемый файл должен вызывать Enter в
// самом начале main (в тестах — в TestMain).
//
// Пространства имён ядро может и не дать: их создание выключено
// настройкой или запрещено контейнером. Тогда процесс запускается без
// них, а Report говорит, без чего именно.
func (c *Command) Start(ctx context.Context) (*Process, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("не найден исполняемый файл ochan: %w", err)
	}
	policy, err := json.Marshal(c.Policy)
	if err != nil {
		return nil, err
	}

	report := Probe(c.Policy)
	attempts := spawnAttempts(c.Policy)
	for i, attempt := range attempts {
		child := exec.CommandContext(ctx, self, childArg, attempt.stage, string(policy))
		child.Env = append(append([]string(nil), c.Env...), envStage+"="+attempt.stage)
		child.Dir = c.Dir
		child.Stdin, child.Stdout, child.Stderr = c.Stdin, c.Stdout, c.Stderr
		child.SysProcAttr = attempt.attr

		err := child.Start()
		if err != nil && i < len(attempts)-1 && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("не удалось запустить процесс песочницы: %w", err)
		}
		report.UserNS, report.NetNS = attempt.userNS, attempt.netNS
		return &Process{cmd: child, Report: report}, nil
	}
	return nil, errors.New("не удалось запустить процесс песочницы")
}

// Enter вызывается первой строкой main. Для обычного запуска сразу
// возвращает false. Для процесса песочницы ставит ограничения и
// возвращает true, когда они действуют: дальше main делает работу
// ребёнка. Если ограничения поставить не удалось, процесс завершается с
// кодом 125 и объяснением в stderr: работать без обещанной изоляции
// хуже, чем не работать.
func Enter() bool {
	if len(os.Args) < 4 || os.Args[1] != childArg || os.Getenv(envStage) != os.Args[2] {
		return false
	}
	var policy Policy
	if err := json.Unmarshal([]byte(os.Args[3]), &policy); err != nil {
		fail(fmt.Errorf("политика испорчена: %w", err))
	}

	switch os.Args[2] {
	case "1":
		// Возвращается только с ошибкой: при успехе процесс уже второй шаг.
		fail(confine(policy, os.Args[3]))
	case "2":
		if err := lockdown(policy); err != nil {
			fail(err)
		}
		os.Unsetenv(envStage)
		return true
	}
	fail(fmt.Errorf("неизвестный шаг %q", os.Args[2]))
	return false
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "песочница: %v\n", err)
	os.Exit(exitFailed)
}

// ExitFailed сообщает, что процесс песочницы завершился, не сумев
// поставить ограничения.
func ExitFailed(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == exitFailed
}
