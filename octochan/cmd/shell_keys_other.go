//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package cmd

// ctrlBackspaceByte — для прочих систем: как у большинства терминалов,
// Ctrl+Backspace — 0x08, Backspace — 0x7F.
func ctrlBackspaceByte() byte { return 0x08 }
