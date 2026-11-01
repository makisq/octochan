//go:build windows

package lang

import (
	"errors"

	"golang.org/x/sys/windows"
)

// impossibleName: Windows отвечает ERROR_INVALID_NAME на имя, которое
// файлом быть не может, — с переводом строки, «"», «<», «|», «?», «*»
// или вторым «:» (первый — ещё имя потока NTFS). Такого файла нет и не
// будет.
func impossibleName(err error) bool { return errors.Is(err, windows.ERROR_INVALID_NAME) }
