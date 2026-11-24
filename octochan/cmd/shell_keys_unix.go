//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package cmd

import (
	"os"

	"golang.org/x/sys/unix"
)

// ctrlBackspaceByte — чем терминал шлёт Ctrl+Backspace: 0x08 (^H), обычный
// Backspace — 0x7F. Но если стирание в терминале настроено на ^H (stty
// erase ^H — так бывает в старых настройках и на некоторых серверах), 0x08
// — это и есть Backspace: тогда Ctrl+Backspace не различить, и он остаётся
// обычным Backspace (0 — без перевода). Слово назад и там — Ctrl+W.
func ctrlBackspaceByte() byte {
	if terminalErase() == 0x08 {
		return 0
	}
	return 0x08
}

// terminalErase — символ стирания терминала (VERASE); 0 — терминала нет.
func terminalErase() byte {
	fd := int(os.Stdin.Fd())
	if tty, err := os.Open("/dev/tty"); err == nil {
		defer tty.Close()
		fd = int(tty.Fd())
	}
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return 0
	}
	return t.Cc[unix.VERASE]
}
