//go:build unix

package rbac

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// CurrentIdentity — кто запустил процесс: uid от ядра, имя — из базы
// пользователей ОС по uid, а не из USER.
func CurrentIdentity() Identity {
	id := Identity{UID: os.Getuid()}
	if u, err := user.LookupId(strconv.Itoa(id.UID)); err == nil {
		id.User = u.Username
	}
	return id
}

// checkSystemFile — файл и каталоги до корня принадлежат root, и никто,
// кроме root, в них не пишет. path уже без символических ссылок.
func checkSystemFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s — не обычный файл", path)
	}
	if err := rootOnly(path, info); err != nil {
		return err
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if err := rootOnly(dir, info); err != nil {
			return err
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}

func rootOnly(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: владельца не узнать", path)
	}
	if st.Uid != 0 {
		return fmt.Errorf("%s принадлежит uid %d, а не root", path, st.Uid)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("в %s может писать не только root (права %04o)", path, info.Mode().Perm())
	}
	return nil
}
