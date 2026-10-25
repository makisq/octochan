//go:build linux && !amd64 && !arm64

package sandbox

import "golang.org/x/sys/unix"

// На других архитектурах фильтр не ставится: номера вызовов и раскладка
// аргументов у них свои, а непроверенный фильтр хуже отсутствующего —
// он обещает защиту, которой нет. Report об этом говорит.
const (
	seccompSupported = false
	auditArch        = 0
	x32Base          = 0
)

var archDenied []uintptr

func buildFilter(Policy, int) []unix.SockFilter { return nil }
