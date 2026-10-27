//go:build linux

package sandbox

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// runPlatform выполняет пробы системных вызовов, которые есть только в Linux.
func runPlatform(p probeReq) (bool, error) {
	switch p.Op {
	case "exec":
		return true, syscall.Exec(p.Path, []string{p.Path}, nil)
	case "kill":
		return true, syscall.Kill(1, 0)
	case "unshare":
		return true, syscall.Unshare(syscall.CLONE_NEWUSER)
	case "socket":
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		if err == nil {
			syscall.Close(fd)
		}
		return true, err
	case "caps":
		header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		var data [2]unix.CapUserData
		if err := unix.Capget(&header, &data[0]); err != nil {
			return true, err
		}
		if data[0].Effective|data[1].Effective|data[0].Permitted|data[1].Permitted != 0 {
			return true, fmt.Errorf("возможности ядра остались: %x %x", data[0].Effective, data[1].Effective)
		}
		return true, nil
	}
	return false, nil
}
