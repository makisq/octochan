//go:build !linux

package sandbox

import (
	"errors"
	"syscall"
)

type attempt struct {
	stage  string
	attr   *syscall.SysProcAttr
	userNS bool
	netNS  bool
	pidNS  bool
}

// На других ОС ограничений ядра нет: ребёнок сразу второй шаг — отдельный
// процесс без окружения родителя.
func spawnAttempts(Policy) []attempt { return []attempt{{stage: "2"}} }

func probe(*Report) {}

func confine(Policy, string) error {
	return errors.New("первый шаг есть только на Linux")
}

func lockdown(Policy) error { return nil }
