//go:build linux && (amd64 || arm64)

package sandbox

import (
	"slices"

	"golang.org/x/sys/unix"
)

// Фильтр seccomp — запреты, а не список разрешённого. Список
// разрешённого пришлось бы сверять с каждой версией рантайма Go, и новая
// версия, позвавшая непривычный вызов, падала бы на ровном месте.
// Запрещается то, чем из процесса выбираются наружу или дотягиваются до
// ядра: запуск программ, отладка чужих процессов, монтирование,
// пространства имён, загрузка в ядро, io_uring (его операции фильтр не
// видит), сигналы чужим процессам, а без сети — и сокеты.

// commonDenied — вызовы, запрещённые на всех архитектурах.
var commonDenied = []uintptr{
	unix.SYS_EXECVE, unix.SYS_EXECVEAT,
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_KCMP,
	unix.SYS_PIDFD_GETFD, unix.SYS_PIDFD_SEND_SIGNAL, unix.SYS_TKILL,
	unix.SYS_RT_SIGQUEUEINFO, unix.SYS_RT_TGSIGQUEUEINFO,
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT,
	unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT, unix.SYS_FSOPEN, unix.SYS_FSCONFIG,
	unix.SYS_FSMOUNT, unix.SYS_FSPICK, unix.SYS_MOUNT_SETATTR,
	unix.SYS_UNSHARE, unix.SYS_SETNS,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
	unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD, unix.SYS_REBOOT,
	unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_ACCT, unix.SYS_QUOTACTL,
	unix.SYS_SETTIMEOFDAY, unix.SYS_CLOCK_SETTIME, unix.SYS_CLOCK_ADJTIME, unix.SYS_ADJTIMEX,
	unix.SYS_SETHOSTNAME, unix.SYS_SETDOMAINNAME, unix.SYS_SYSLOG, unix.SYS_VHANGUP,
	unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT, unix.SYS_LOOKUP_DCOOKIE,
	unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	unix.SYS_FANOTIFY_INIT, unix.SYS_MIGRATE_PAGES, unix.SYS_MOVE_PAGES,
}

// namespaceFlags — флаги clone, создающие пространства имён.
const namespaceFlags = unix.CLONE_NEWNS | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER |
	unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWCGROUP

// Смещения в struct seccomp_data: номер вызова, архитектура, аргументы.
// Аргументы 64-битные; сравнивается младшее слово, оно на little-endian
// первое. Для pid и номера ioctl этого достаточно: ядро само берёт у них
// 32 бита.
const (
	offsetNr   = 0
	offsetArch = 4
	offsetArg0 = 16
	offsetArg1 = 24
)

const (
	retAllow = unix.SECCOMP_RET_ALLOW
	retKill  = unix.SECCOMP_RET_KILL_PROCESS
	retDeny  = unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
	// clone3 — ENOSYS: его аргументы фильтр прочитать не может, а на
	// ENOSYS рантаймы сами переходят на clone, который фильтр видит.
	retNoSys = unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)
)

func stmt(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }

func jump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

const (
	ldW  = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
	jeq  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
	jge  = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
	jset = unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K
	ret  = unix.BPF_RET | unix.BPF_K
)

// buildFilter строит программу BPF. pid — свой процесс: сигналы можно
// слать только ему.
func buildFilter(policy Policy, pid int) []unix.SockFilter {
	f := []unix.SockFilter{
		// Чужая архитектура — другие номера вызовов: такой вызов убивает
		// процесс, а не проходит мимо фильтра.
		stmt(ldW, offsetArch),
		jump(jeq, auditArch, 1, 0),
		stmt(ret, retKill),
		stmt(ldW, offsetNr),
	}
	if x32Base != 0 {
		f = append(f, jump(jge, x32Base, 0, 1), stmt(ret, retDeny))
	}

	denied := append(append([]uintptr(nil), commonDenied...), archDenied...)
	if !policy.Network {
		denied = append(denied, unix.SYS_SOCKET)
	}
	if len(policy.Exec) > 0 {
		// Запуск программ: execve пропускается, а что запускать, решает
		// Landlock — у него есть путь файла, у seccomp нет.
		denied = slices.DeleteFunc(denied, func(nr uintptr) bool {
			return nr == unix.SYS_EXECVE || nr == unix.SYS_EXECVEAT
		})
	}
	for _, nr := range denied {
		f = append(f, jump(jeq, uint32(nr), 0, 1), stmt(ret, retDeny))
	}
	f = append(f, jump(jeq, unix.SYS_CLONE3, 0, 1), stmt(ret, retNoSys))

	// clone — только без новых пространств имён.
	f = append(f,
		jump(jeq, unix.SYS_CLONE, 0, 4),
		stmt(ldW, offsetArg0),
		jump(jset, namespaceFlags, 0, 1),
		stmt(ret, retDeny),
		stmt(ret, retAllow),
	)

	// ioctl — без TIOCSTI и TIOCLINUX: ими пишут в чужой терминал, как с
	// клавиатуры.
	f = append(f,
		jump(jeq, unix.SYS_IOCTL, 0, 5),
		stmt(ldW, offsetArg1),
		jump(jeq, unix.TIOCSTI, 2, 0),
		jump(jeq, unix.TIOCLINUX, 1, 0),
		stmt(ret, retAllow),
		stmt(ret, retDeny),
	)

	// kill и tgkill — только себе: рантайм Go шлёт сигналы своим потокам.
	// С запуском программ процессы живут в своём пространстве имён PID:
	// сигналы и так не выходят наружу, а остановить запущенную программу
	// по истечении времени процессу песочницы нужно.
	if len(policy.Exec) > 0 {
		return append(f, stmt(ret, retAllow))
	}
	for _, nr := range []uintptr{unix.SYS_KILL, unix.SYS_TGKILL} {
		f = append(f,
			jump(jeq, uint32(nr), 0, 4),
			stmt(ldW, offsetArg0),
			jump(jeq, uint32(pid), 1, 0),
			stmt(ret, retDeny),
			stmt(ret, retAllow),
		)
	}

	return append(f, stmt(ret, retAllow))
}
