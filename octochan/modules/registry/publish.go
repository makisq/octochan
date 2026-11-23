package registry

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Публикация — в реестр-каталог: его потом отдают как есть по HTTP или
// коммитят в git. Версия неизменна: опубликованную с другим содержимым
// заменить нельзя — только опубликовать новую или отозвать.

// Meta — что пакет говорит о себе.
type Meta struct {
	Description string
	Modes       []string
	Requires    []string
}

// Publish кладёт архив версии в реестр и переписывает индекс. key — ключ
// подписи: есть — индекс подписывается заново.
func Publish(dir, name, version string, meta Meta, archive []byte, files int, key ed25519.PrivateKey, now time.Time) (string, error) {
	idx, err := loadLocal(dir)
	if err != nil {
		return "", err
	}
	for _, r := range meta.Requires {
		if _, err := ParseRequire(r); err != nil {
			return "", err
		}
	}
	file := fmt.Sprintf("%s-%s.tar.gz", name, version)
	sum := Sum(archive)
	e := idx.Packages[name]
	if e == nil {
		e = &Entry{Versions: map[string]*Version{}}
		idx.Packages[name] = e
	}
	if old, ok := e.Versions[version]; ok {
		if old.SHA256 == sum {
			return file, nil
		}
		return "", fmt.Errorf("@%s v%s уже опубликована с другим содержимым (sha256 %s): версия неизменна — поднимите version в package.yaml",
			name, version, Short(old.SHA256))
	}
	if meta.Description != "" {
		e.Description = meta.Description
	}
	e.Versions[version] = &Version{Archive: file, SHA256: sum, Files: files, Modes: meta.Modes,
		Requires: meta.Requires, Protocol: Protocol, Published: now.UTC().Truncate(time.Second)}
	if err := os.WriteFile(filepath.Join(dir, file), archive, 0o644); err != nil {
		return "", err
	}
	return file, saveLocal(dir, idx, key)
}

// Revoke отзывает версию: она остаётся в индексе с причиной, и ochan её
// не поставит.
func Revoke(dir, name, version, reason string, key ed25519.PrivateKey) error {
	if reason == "" {
		return errors.New("причина отзыва обязательна: её увидят все, кто попробует поставить версию")
	}
	idx, err := loadLocal(dir)
	if err != nil {
		return err
	}
	e := idx.Packages[name]
	if e == nil || e.Versions[version] == nil {
		return fmt.Errorf("@%s v%s в реестре %s нет", name, version, dir)
	}
	e.Versions[version].Revoked = reason
	return saveLocal(dir, idx, key)
}

func loadLocal(dir string) (*Index, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("реестр %s — не каталог", dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, IndexName))
	if errors.Is(err, os.ErrNotExist) {
		return &Index{Protocol: Protocol, Packages: map[string]*Entry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseIndex(data)
}

func saveLocal(dir string, idx *Index, key ed25519.PrivateKey) error {
	data, err := idx.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, IndexName), data, 0o644); err != nil {
		return err
	}
	sigPath := filepath.Join(dir, IndexSig)
	if key == nil {
		// Индекс поменялся без ключа: прежняя подпись к нему уже не
		// подходит, и оставить её — значит оставить ложь.
		if err := os.Remove(sigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.WriteFile(sigPath, Sign(key, data), 0o644)
}
