//go:build !windows

package lang

// impossibleName: в Unix имя файла — любые байты, кроме «/» и нуля, и
// невозможного имени здесь не бывает.
func impossibleName(error) bool { return false }
