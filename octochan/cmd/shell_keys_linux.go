//go:build linux

package cmd

import "golang.org/x/sys/unix"

// ioctlGetTermios — запрос режима терминала в Linux.
const ioctlGetTermios = unix.TCGETS
