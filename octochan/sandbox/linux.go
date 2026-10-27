//go:build linux

package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

type attempt struct {
	stage  string
	attr   *syscall.SysProcAttr
	userNS bool
	netNS  bool
	pidNS  bool
}

// spawnAttempts — способы запуска от лучшего к худшему. Свой
// пользователь не даёт привилегиям снаружи действовать внутри; своё
// сетевое пространство — пустое, без интерфейсов. Если ядро не даёт
// пространство пользователя (выключено настройкой или запрещено
// контейнером), root ещё может получить сетевое пространство без него.
//
// С запуском программ (policy.Exec) запасных способов нет: процессы
// живут в своём пространстве имён PID — сигналы не уходят наружу, а с
// процессом песочницы (там он init) гибнут все запущенные им. Не дало
// ядро этих пространств — песочница не запускается.
func spawnAttempts(policy Policy) []attempt {
	ns := uintptr(syscall.CLONE_NEWIPC)
	if !policy.Network {
		ns |= syscall.CLONE_NEWNET
	}
	uid, gid := os.Getuid(), os.Getgid()
	if len(policy.Exec) > 0 {
		return []attempt{{
			stage: "1",
			attr: &syscall.SysProcAttr{
				Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | ns,
				UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
				GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
				GidMappingsEnableSetgroups: false,
				Pdeathsig:                  syscall.SIGKILL,
			},
			userNS: true,
			netNS:  !policy.Network,
			pidNS:  true,
		}}
	}

	list := []attempt{{
		stage: "1",
		attr: &syscall.SysProcAttr{
			Cloneflags:                 syscall.CLONE_NEWUSER | ns,
			UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
			GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
			GidMappingsEnableSetgroups: false,
			Pdeathsig:                  syscall.SIGKILL,
		},
		userNS: true,
		netNS:  !policy.Network,
	}}
	if os.Geteuid() == 0 {
		list = append(list, attempt{
			stage: "1",
			attr:  &syscall.SysProcAttr{Cloneflags: ns, Pdeathsig: syscall.SIGKILL},
			netNS: !policy.Network,
		})
	}
	return append(list, attempt{stage: "1", attr: &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}})
}

func probe(report *Report) {
	report.Landlock = landlockABI()
	report.Seccomp = seccompSupported && seccompAvailable()
}

// ---------------------------------------------------------------------------
// Шаг 1: ограничить поток и сделать exec
// ---------------------------------------------------------------------------

// confine ставит no_new_privs и Landlock на текущий поток и заменяет
// образ процесса вторым шагом. При успехе не возвращается.
func confine(policy Policy, raw string) error {
	// Всё дальше — на одном потоке: ограничения Landlock принадлежат
	// потоку, и exec должен сделать именно он.
	runtime.LockOSThread()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("не найден исполняемый файл: %w", err)
	}
	// Для exec нужны сам файл и всё, что он подгружает (у сборки с cgo —
	// загрузчик и libc): их пути — в карте памяти процесса.
	exec, err := mappedFiles()
	if err != nil {
		return err
	}
	exec = append(exec, exe)
	exec = append(exec, policy.Exec...)

	// Снимок памяти может унести секреты родителя наружу.
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{})

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := dropRoot(); err != nil {
		return err
	}
	if abi := landlockABI(); abi > 0 {
		if err := restrictFiles(abi, policy, exec); err != nil {
			return fmt.Errorf("Landlock: %w", err)
		}
	}

	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, envStage+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, envStage+"=2")
	if policy.Network {
		// Свой резолвер Go читает только resolv.conf и hosts; резолвер
		// libc подгрузил бы модули NSS, которых в песочнице нет.
		env = append(env, "GODEBUG=netdns=go")
	}
	err = syscall.Exec(exe, []string{exe, childArg, "2", raw}, env)
	return fmt.Errorf("exec второго шага: %w", err)
}

// dropRoot лишает root привилегий: после exec он остаётся root по номеру,
// но без единой возможности ядра (capabilities). Под root песочница иначе
// держала бы только Landlock и seccomp, а root внутри ещё мог бы,
// например, перенастроить сеть хоста.
func dropRoot() error {
	if os.Geteuid() != 0 {
		// Обычный пользователь теряет все возможности на exec сам, а
		// возможности из атрибутов файла no_new_privs не даёт.
		return nil
	}
	for capability := 0; capability <= 63; capability++ {
		err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capability), 0, 0, 0)
		if err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("сброс привилегий root: %w", err)
		}
	}
	_ = unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0)

	// SECBIT_NOROOT: root не получает возможностей на exec. Биты
	// закреплены, чтобы их нельзя было вернуть.
	const secureBits = 0x01 | 0x02 | 0x04 | 0x08 | 0x20 | 0x40 | 0x80
	if err := unix.Prctl(unix.PR_SET_SECUREBITS, secureBits, 0, 0, 0); err != nil {
		return fmt.Errorf("сброс привилегий root: %w", err)
	}
	return nil
}

// mappedFiles — файлы, отображённые в память процесса.
func mappedFiles() ([]string, error) {
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		return nil, fmt.Errorf("карта памяти процесса: %w", err)
	}
	defer f.Close()

	seen := map[string]bool{}
	var files []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 || fields[4] == "0" || !strings.HasPrefix(fields[5], "/") {
			continue
		}
		path := strings.Join(fields[5:], " ")
		if strings.HasSuffix(path, " (deleted)") || seen[path] {
			continue
		}
		seen[path] = true
		files = append(files, path)
	}
	return files, scanner.Err()
}

// ---------------------------------------------------------------------------
// Landlock
// ---------------------------------------------------------------------------

func landlockABI() int {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(abi)
}

// Права файловой системы Landlock.
const (
	fsV1 = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM

	// Каталоги сценария: читать, писать, создавать и удалять файлы и
	// каталоги. Ни исполнять, ни создавать ссылки, устройства и сокеты.
	rightsWrite = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE
	rightsRead = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	rightsExec = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_EXECUTE

	// rightsFile — права, применимые к файлу, а не каталогу.
	rightsFile = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE |
		unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// handledFS — права, которые ядро этой версии умеет запрещать. Всё, что
// ядро умеет запрещать, запрещается, кроме выданного правилами.
func handledFS(abi int) uint64 {
	rights := uint64(fsV1)
	if abi >= 2 {
		rights |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		rights |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		rights |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return rights
}

// systemRead — системные файлы, без которых процесс работает хуже, а
// секретов в них нет: часовые пояса и кэш загрузчика.
var systemRead = []string{"/etc/localtime", "/usr/share/zoneinfo", "/etc/ld.so.cache", "/etc/ld.so.preload"}

// execLibraries — каталоги библиотек для запущенных программ.
var execLibraries = []string{"/lib", "/lib32", "/lib64", "/usr/lib", "/usr/lib32", "/usr/lib64", "/usr/local/lib"}

// execDevices — устройства без секретов, которые открывают почти все
// программы.
var execDevices = []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/random"}

// networkRead — что читает сетевой клиент: резолвер и корневые
// сертификаты. Только при разрешённой сети.
var networkRead = []string{
	"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/gai.conf", "/etc/host.conf",
	"/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/usr/share/ca-certificates",
	"/usr/local/share/ca-certificates",
}

func restrictFiles(abi int, policy Policy, exec []string) error {
	handled := handledFS(abi)
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	if abi >= 4 {
		// Слушать порты сценарию незачем никогда; подключаться — только
		// к живым источникам.
		attr.Access_net = unix.LANDLOCK_ACCESS_NET_BIND_TCP
		if !policy.Network {
			attr.Access_net |= unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
		}
	}
	if abi >= 6 {
		// Ни сигналов, ни абстрактных сокетов UNIX за пределами песочницы.
		attr.Scoped = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
	}

	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("набор правил: %w", errno)
	}
	defer unix.Close(int(ruleset))

	for _, dir := range policy.Write {
		if err := addRule(int(ruleset), dir, rightsWrite&handled, true); err != nil {
			return err
		}
	}
	read := append(append([]string(nil), policy.Read...), systemRead...)
	if policy.Network {
		read = append(read, networkRead...)
	}
	for _, path := range read {
		if err := addRule(int(ruleset), path, rightsRead&handled, false); err != nil {
			return err
		}
	}
	for _, path := range exec {
		if err := addRule(int(ruleset), path, rightsExec&handled, false); err != nil {
			return err
		}
	}
	if len(policy.Exec) > 0 {
		// Запущенной программе нужны библиотеки (их подгружает загрузчик
		// ELF: чтение, не запуск) и /dev/null. Секретов там нет.
		for _, path := range execLibraries {
			if err := addRule(int(ruleset), path, rightsRead&handled, false); err != nil {
				return err
			}
		}
		for _, path := range execDevices {
			if err := addRule(int(ruleset), path, (unix.LANDLOCK_ACCESS_FS_READ_FILE|unix.LANDLOCK_ACCESS_FS_WRITE_FILE)&handled, false); err != nil {
				return err
			}
		}
	}

	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, ruleset, 0, 0); errno != 0 {
		return fmt.Errorf("ограничение процесса: %w", errno)
	}
	return nil
}

// addRule разрешает права внутри path. Необязательного пути может не
// быть: часовых поясов, например, нет в минимальных контейнерах.
func addRule(ruleset int, path string, rights uint64, required bool) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("путь %s не абсолютный", path)
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if !required && errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	defer unix.Close(fd)

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		rights &= rightsFile
	}
	rule := unix.LandlockPathBeneathAttr{Allowed_access: rights, Parent_fd: int32(fd)}
	_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset),
		unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("правило для %s: %w", path, errno)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Шаг 2: seccomp
// ---------------------------------------------------------------------------

func seccompAvailable() bool {
	_, err := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0)
	return err == nil
}

// lockdown ставит фильтр системных вызовов сразу на все потоки.
func lockdown(policy Policy) error {
	if !seccompSupported || !seccompAvailable() {
		return nil
	}
	filter := buildFilter(policy, os.Getpid())
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	thread, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog)))
	runtime.KeepAlive(filter)
	switch {
	case errno != 0:
		return fmt.Errorf("seccomp: %w", errno)
	case thread != 0:
		return fmt.Errorf("seccomp: поток %d не принял фильтр", thread)
	}
	return nil
}
