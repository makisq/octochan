//go:build unix

package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// checkPrivate проверяет вынесенный каталог так же, как sshd — ключи: сам
// каталог и каждый каталог над ним принадлежат вам или root и закрыты для
// записи группе и остальным. Каталог над ним с липким битом (/tmp) годится:
// чужое в нём не переименовать. Иначе другой пользователь машины подменил бы
// каталог или подложил в него файл — например, плагин в модули.
func checkPrivate(dir string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	uid := uint32(os.Getuid())
	for path, self := real, true; ; self = false {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != 0 && stat.Uid != uid {
			return fmt.Errorf("каталог %s принадлежит другому пользователю (uid %d): он мог бы подменить в нём файлы — "+
				"нужен каталог ваш или root", path, stat.Uid)
		}
		if mode := info.Mode(); mode.Perm()&0o022 != 0 && (self || mode&os.ModeSticky == 0) {
			return fmt.Errorf("каталог %s открыт для записи группе или остальным (%s): другой пользователь мог бы "+
				"подменить в нём файлы — закройте: chmod go-w %s", path, mode.Perm(), path)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}
