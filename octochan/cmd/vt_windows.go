//go:build windows

package cmd

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVirtualTerminal включает в консоли Windows разбор ANSI-последователь-
// ностей. Без него классическая консоль печатает коды цвета как текст.
// false — консоль режим не поддерживает (или это не консоль), и выводить
// нужно без цвета.
func enableVirtualTerminal(f *os.File) bool {
	handle := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
