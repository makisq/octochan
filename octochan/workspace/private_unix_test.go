//go:build unix

package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Каталог, открытый для записи остальным, — замечание: и сам, и каталог над
// ним; закрытый — без замечаний.
func TestCheckPrivate(t *testing.T) {
	root := t.TempDir()
	note := func(dir string) string {
		t.Helper()
		got, err := checkPrivate(dir, true)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := note(root); got != "" {
		t.Skipf("временный каталог сам не закрыт: %s", got)
	}
	open := filepath.Join(root, "open")
	inner := filepath.Join(open, "modules")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := note(inner); got != "" {
		t.Fatalf("замечание о закрытом каталоге: %s", got)
	}
	// Режим ставится явно: umask его не сузит.
	if err := os.Chmod(inner, 0o777); err != nil {
		t.Fatal(err)
	}
	if got := note(inner); !strings.Contains(got, "chmod go-w "+inner) {
		t.Fatalf("о каталоге 0777 нет замечания: %q", got)
	}
	if err := os.Chmod(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o775); err != nil {
		t.Fatal(err)
	}
	if got := note(inner); !strings.Contains(got, open) {
		t.Fatalf("о каталоге под открытым для группы нет замечания: %q", got)
	}
	// Каталога ещё нет — смотрят на каталоги над ним.
	missing := filepath.Join(inner, "нет", "ещё")
	if got, err := OpenToOthers(missing); err != nil || !strings.Contains(got, "chmod go-w "+open) {
		t.Fatalf("о каталоге, которого ещё нет, под открытым: %q, %v", got, err)
	}
	// Липкий бит у каталога выше — как у /tmp: чужое в нём не переименовать.
	if err := os.Chmod(open, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if got := note(inner); got != "" {
		t.Fatalf("замечание о каталоге под липким: %s", got)
	}
	// Новый каталог под липким будет создан закрытым; сам липкий, открытый
	// для записи, рабочим каталогом — открыт.
	if got, err := OpenToOthers(filepath.Join(open, "новый")); err != nil || got != "" {
		t.Fatalf("о новом каталоге под липким: %q, %v", got, err)
	}
	if got, err := OpenToOthers(open); err != nil || !strings.Contains(got, "chmod go-w "+open) {
		t.Fatalf("о самом липком каталоге нет замечания: %q, %v", got, err)
	}
}

// Открытый каталог — не отказ: ochan работает, а замечание называет, откуда
// взят путь. Два каталога под одним открытым — одно замечание.
func TestOpenDirsWarnNotFail(t *testing.T) {
	root := t.TempDir()
	if got, err := checkPrivate(root, true); err != nil || got != "" {
		t.Skipf("временный каталог сам не закрыт: %s %v", got, err)
	}
	clearPathEnv(t, t.TempDir())

	// Вынесенный каталог модулей открыт для всех.
	modules := filepath.Join(root, "modules")
	if err := os.MkdirAll(modules, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(modules, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DirModules.Env(), modules)
	fm, err := NewFileManager()
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.Ensure(); err != nil {
		t.Fatalf("открытый каталог модулей — отказ: %v", err)
	}
	notes := fm.OpenDirs()
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "окружение OCTOCHAN_MODULES: ") || !strings.Contains(notes[0], "chmod go-w "+modules) {
		t.Fatalf("замечания: %q", notes)
	}
	// Повторный Ensure замечаний не удваивает.
	if err := fm.Ensure(); err != nil || len(fm.OpenDirs()) != 1 {
		t.Fatalf("после второго Ensure: %q, %v", fm.OpenDirs(), err)
	}

	// Рабочий каталог в общей папке (0770) — тоже замечание, а не отказ.
	shared := filepath.Join(root, "shared")
	home := filepath.Join(shared, "state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o770); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, home)
	t.Setenv(DirModules.Env(), filepath.Join(shared, "modules"))
	fm, err = NewFileManager()
	if err != nil {
		t.Fatalf("рабочий каталог в общей папке — отказ: %v", err)
	}
	if err := fm.Ensure(); err != nil {
		t.Fatal(err)
	}
	notes = fm.OpenDirs()
	if len(notes) != 1 || !strings.HasPrefix(notes[0], EnvHome+": ") || !strings.Contains(notes[0], "chmod go-w "+shared) ||
		!strings.Contains(notes[0], "-rwxrwx---") {
		t.Fatalf("замечания о рабочем каталоге в общей папке: %q", notes)
	}

	// Рабочий каталог по умолчанию не проверяется, закрытый — без замечаний.
	t.Setenv(EnvHome, "")
	t.Setenv(DirModules.Env(), "")
	fm, err = NewFileManager()
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.Ensure(); err != nil || len(fm.OpenDirs()) != 0 {
		t.Fatalf("замечания без открытых каталогов: %q, %v", fm.OpenDirs(), err)
	}
}
