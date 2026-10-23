package lang

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"octochan/config"
)

// sandboxed выполняет исходник в песочнице с корнем root.
func sandboxed(t *testing.T, root string, env, live []string, src string) (string, error) {
	t.Helper()

	box, err := NewSandbox([]string{root}, env, live)
	if err != nil {
		t.Fatal(err)
	}
	file, err := Parse("<тест>", src)
	if err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	interp := NewInterpreter(context.Background(), NewRegistry(), &out)
	interp.SetSandbox(box)
	interp.SetDir(root)
	err = interp.RunFile(file)
	return out.String(), err
}

// quote записывает путь строкой языка: обратные косые черты Windows
// внутри кавычек иначе стали бы экранированием.
func quote(path string) string {
	return `"` + strings.ReplaceAll(path, `\`, `\\`) + `"`
}

func mustFail(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ожидалась ошибка с %q, выполнение прошло", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("ожидалась ошибка с %q, получено: %v", want, err)
	}
}

func TestSandboxFilesInsideRootWork(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.conf"), []byte("port = 5432\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sandboxed(t, root, nil, nil, `
c = config("a.conf")
print(c["port"], exists("a.conf"), exists("нет.conf"))
write("out.txt", "привет")
put(c, "port", "6432")
save(c, "b.conf")
print(read("out.txt"), param("b.conf", "port"))
`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "5432 true false\nпривет 6432\n"; out != want {
		t.Fatalf("вывод %q, ожидалось %q", out, want)
	}
}

func TestSandboxRejectsPathsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.conf")
	if err := os.WriteFile(secret, []byte("password = x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"read абсолютного пути": "read(" + quote(secret) + ")",
		"read через ..":         `read("../` + filepath.Base(outside) + `/secret.conf")`,
		"config":                "config(" + quote(secret) + ")",
		"diff":                  "diff(" + quote(secret) + `, "port = 1\n")`,
		"param":                 "param(" + quote(secret) + `, "password")`,
		"exists":                "exists(" + quote(secret) + ")",
		"write":                 "write(" + quote(filepath.Join(outside, "new.txt")) + `, "x")`,
		"save":                  `save(config("a = 1\n"), ` + quote(filepath.Join(outside, "new.conf")) + ")",
		"в parallel for":        "parallel for p in [" + quote(secret) + "] { read(p) }",
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := sandboxed(t, root, nil, nil, src)
			mustFail(t, err, "вне разрешённых каталогов")
		})
	}

	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("запись за пределы песочницы всё-таки состоялась")
	}
}

// Ссылка внутри разрешённого каталога, ведущая наружу, — главный способ
// обойти проверку пути, если её делать по тексту.
func TestSandboxResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("создание символических ссылок на Windows требует прав")
	}

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.conf"), []byte("password = x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.conf"), filepath.Join(root, "link.conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "missing.txt"), filepath.Join(root, "dangling.txt")); err != nil {
		t.Fatal(err)
	}

	_, err := sandboxed(t, root, nil, nil, `read("link.conf")`)
	mustFail(t, err, "вне разрешённых каталогов")

	_, err = sandboxed(t, root, nil, nil, `write("dir/new.txt", "x")`)
	mustFail(t, err, "вне разрешённых каталогов")

	_, err = sandboxed(t, root, nil, nil, `write("dangling.txt", "x")`)
	mustFail(t, err, "никуда не ведёт")

	if _, err := os.Stat(filepath.Join(outside, "missing.txt")); !os.IsNotExist(err) {
		t.Fatal("запись по висячей ссылке создала файл снаружи")
	}
}

func TestSandboxEnvWhitelist(t *testing.T) {
	root := t.TempDir()
	t.Setenv("OCHAN_TEST_ALLOWED", "да")
	t.Setenv("OCHAN_TEST_SECRET", "токен")

	out, err := sandboxed(t, root, []string{"OCHAN_TEST_ALLOWED"}, nil, `print(env("OCHAN_TEST_ALLOWED"))`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "да\n" {
		t.Fatalf("вывод %q", out)
	}

	// Значение по умолчанию не подменяет отказ: сценарий, получивший пустую
	// строку вместо пароля, сломался бы дальше и непонятно где.
	_, err = sandboxed(t, root, []string{"OCHAN_TEST_ALLOWED"}, nil, `print(env("OCHAN_TEST_SECRET", "x"))`)
	mustFail(t, err, "OCHAN_TEST_SECRET недоступна, разрешены: OCHAN_TEST_ALLOWED")

	_, err = sandboxed(t, root, nil, nil, `env("OCHAN_TEST_ALLOWED")`)
	mustFail(t, err, "не разрешена ни одна")
}

func TestSandboxLiveSources(t *testing.T) {
	root := t.TempDir()
	config.RegisterSourceScheme("ochantest", func(_ context.Context, _ string, _ config.SourceOptions) (*config.LoadedConfig, error) {
		return &config.LoadedConfig{Name: "ochantest://db1", Data: config.ConfigMap{"": {"port": "5432"}}}, nil
	})

	_, err := sandboxed(t, root, nil, nil, `config("ochantest://user:pw@db1")`)
	mustFail(t, err, "живые источники в песочнице отключены")

	_, err = sandboxed(t, root, nil, []string{"other"}, `diff("ochantest://db1", "port = 1\n")`)
	mustFail(t, err, "схема ochantest не разрешена, разрешены: other")

	out, err := sandboxed(t, root, nil, []string{"ochantest"}, `print(config("ochantest://db1")["port"])`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "5432\n" {
		t.Fatalf("вывод %q", out)
	}
}

// Многострочная строка — это текст конфига, а не путь: песочница её
// не трогает.
func TestSandboxAllowsInlineConfigText(t *testing.T) {
	out, err := sandboxed(t, t.TempDir(), nil, nil, `print(diff("a = 1\n", "a = 2\n"))`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "1\n" {
		t.Fatalf("вывод %q", out)
	}
}

func TestNewSandboxNeedsExistingRoot(t *testing.T) {
	if _, err := NewSandbox(nil, nil, nil); err == nil {
		t.Fatal("песочница без каталогов создалась")
	}
	if _, err := NewSandbox([]string{filepath.Join(t.TempDir(), "нет")}, nil, nil); err == nil {
		t.Fatal("песочница с несуществующим каталогом создалась")
	}
}
