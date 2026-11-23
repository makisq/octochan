//go:build linux

package sandbox

import "golang.org/x/sys/unix"

const (
	seccompSupported = true
	auditArch        = unix.AUDIT_ARCH_X86_64
	// x32Base — начало номеров ABI x32: те же вызовы под другими
	// номерами, мимо списка запретов. Запрещены все.
	x32Base = 0x40000000
)

// archDenied — вызовы, которые есть только на x86-64.
var archDenied = []uintptr{
	unix.SYS_IOPL, unix.SYS_IOPERM, unix.SYS_MODIFY_LDT, unix.SYS_USELIB,
	unix.SYS_NFSSERVCTL, unix.SYS_CREATE_MODULE, unix.SYS_GET_KERNEL_SYMS,
}
