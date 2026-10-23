//go:build unix

package audit

import (
	"os"

	"golang.org/x/sys/unix"
)

type fileLock struct{ f *os.File }

// lockFile берёт исключительный замок: процессы сервера пишут в журнал
// параллельно, а цепочке нужен строгий порядок.
func lockFile(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) unlock() {
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	l.f.Close()
}
