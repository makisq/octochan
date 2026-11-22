//go:build unix

package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// checkPrivate смотрит на каталог так же, как sshd — на ключи: сам каталог и
// каждый каталог над ним принадлежат вам или root и закрыты для записи
// группе и остальным. Каталог над ним с липким битом (/tmp) годится: чужое в
// нём не переименовать. Иначе другой пользователь машины подменил бы каталог
// или подложил в него файл — например, плагин в модули.
//
// Возвращает замечание — что открыто и чем закрыть; пустое — каталог закрыт.
// Это замечание, а не отказ: закрыть каталог можно не всегда (общая папка,
// чужой владелец), и решает здесь хозяин установки. self — проверяется сам
// каталог dir; нет — каталог над тем, которого ещё нет.
func checkPrivate(dir string, self bool) (string, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("%s: %w", dir, err)
	}
	uid := uint32(os.Getuid())
	for path := real; ; self = false {
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != 0 && stat.Uid != uid {
			return fmt.Sprintf("каталог %s принадлежит другому пользователю (uid %d): он мог бы подменить в нём файлы — "+
				"надёжнее каталог ваш или root", path, stat.Uid), nil
		}
		if mode := info.Mode(); mode.Perm()&0o022 != 0 && (self || mode&os.ModeSticky == 0) {
			return fmt.Sprintf("каталог %s открыт для записи группе или остальным (%s): другой пользователь мог бы "+
				"подменить в нём файлы — закрыть: chmod go-w %s", path, mode.Perm(), path), nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", nil
		}
		path = parent
	}
}
