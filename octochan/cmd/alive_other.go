//go:build !unix

package cmd

import "os"

// processAlive — процесс с этим номером есть: на Windows FindProcess
// открывает процесс и без него отказывает.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
