//go:build windows

package cmd

// ctrlBackspaceByte — чем консоль Windows шлёт Ctrl+Backspace: 0x7F (DEL),
// обычный Backspace — 0x08. Так и у классической консоли, и у Windows
// Terminal: ввод приходит записями консоли, и go-tty отдаёт их символ.
func ctrlBackspaceByte() byte { return 0x7f }
