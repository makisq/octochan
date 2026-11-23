//go:build linux

package sandbox

import "golang.org/x/sys/unix"

const (
	seccompSupported = true
	auditArch        = unix.AUDIT_ARCH_AARCH64
	x32Base          = 0
)

// archDenied — вызовы, которые есть только на arm64.
var archDenied = []uintptr{unix.SYS_NFSSERVCTL}
