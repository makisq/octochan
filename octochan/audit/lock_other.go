//go:build !unix && !windows

package audit

// На прочих системах замка нет: ochan там работает одним процессом.
type fileLock struct{}

func lockFile(string) (*fileLock, error) { return &fileLock{}, nil }

func (*fileLock) unlock() {}
