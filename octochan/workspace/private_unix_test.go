//go:build unix

package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Вынесенный каталог, открытый для записи остальным, — отказ: и сам, и
// каталог над ним; закрытый — годится.
func TestCheckPrivate(t *testing.T) {
	root := t.TempDir()
	if err := checkPrivate(root); err != nil {
		t.Skipf("временный каталог сам не закрыт: %v", err)
	}
	open := filepath.Join(root, "open")
	inner := filepath.Join(open, "modules")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivate(inner); err != nil {
		t.Fatalf("закрытый каталог отвергнут: %v", err)
	}
	// Режим ставится явно: umask его не сузит.
	if err := os.Chmod(inner, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivate(inner); err == nil || !strings.Contains(err.Error(), "chmod go-w "+inner) {
		t.Fatalf("каталог 0777 принят: %v", err)
	}
	if err := os.Chmod(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivate(inner); err == nil || !strings.Contains(err.Error(), open) {
		t.Fatalf("каталог под открытым для группы принят: %v", err)
	}
	// Липкий бит у каталога выше — как у /tmp: чужое в нём не переименовать.
	if err := os.Chmod(open, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivate(inner); err != nil {
		t.Fatalf("каталог под липким отвергнут: %v", err)
	}

	// Ensure проверяет вынесенный каталог и называет настройку.
	home := t.TempDir()
	clearPathEnv(t, home)
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(inner, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DirModules.Env(), inner)
	fm, err := NewFileManager()
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.Ensure(); err == nil || !strings.Contains(err.Error(), "окружение OCTOCHAN_MODULES") {
		t.Fatalf("открытый каталог модулей принят: %v", err)
	}
}
