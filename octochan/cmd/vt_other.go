//go:build !windows

package cmd

import "os"

// enableVirtualTerminal: терминалы Unix разбирают ANSI-последовательности
// сами, включать нечего.
func enableVirtualTerminal(*os.File) bool { return true }
