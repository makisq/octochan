//go:build !unix

package rbac

import (
	"errors"
	"os/user"
)

// CurrentIdentity — пользователь по данным ОС; uid на этой системе нет.
func CurrentIdentity() Identity {
	id := Identity{UID: -1}
	if u, err := user.Current(); err == nil {
		id.User = u.Username
	}
	return id
}

// checkSystemFile: владельца файла здесь так не проверить — системной
// политике не верим.
func checkSystemFile(string) error {
	return errors.New("системная политика ролей проверяется только на Linux, macOS и BSD")
}
