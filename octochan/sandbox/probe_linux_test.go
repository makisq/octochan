//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
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
	case "run":
		return true, exec.Command(p.Path).Run()
	case "copy-run":
		// Программу скопировали в каталог, куда сценарий пишет: ядро не
		// даёт запустить копию, хотя оригинал разрешён.
		data, err := os.ReadFile(p.Path)
		if err != nil {
			return true, err
		}
		if err := os.WriteFile(p.Addr, data, 0o700); err != nil {
			return true, err
		}
		return true, exec.Command(p.Addr).Run()
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
