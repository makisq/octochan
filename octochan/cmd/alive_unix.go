//go:build unix

package cmd

import (
	"errors"
	"syscall"
)

// processAlive — процесс с этим номером есть: сигнал 0 ничего не делает,
// только проверяет.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
