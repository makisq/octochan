//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package cmd

import "golang.org/x/sys/unix"

// ioctlGetTermios — запрос режима терминала в macOS и BSD.
const ioctlGetTermios = unix.TIOCGETA
