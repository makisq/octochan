package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// Пакет из реестра ставится заявкой: план, «да», apply --request — ровно
// согласованный архив; отозванная версия и чужие источники — отказ.
func TestRegistryInstall(t *testing.T) {
	e := newScriptEnv(t)
	reg := t.TempDir()
	pkg := filepath.Join(e.work, "pgtools")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(pkg, "pgtools.ochan"), "alias лимит(c: config, n: int) {\n    put(c, \"max_connections\", str(n))\n    return c\n}\n")
	manifest := func(v string) {
		e.write(filepath.Join(pkg, "package.yaml"), "version: "+v+"\ndescription: правила для postgresql.conf\nmodes: postgresql.conf\n")
	}
	if out := e.must("!!registry", "keygen", filepath.Join(e.work, "rel")); !strings.Contains(out, "rel: ") {
		t.Fatalf("keygen:\n%s", out)
	}
	pub, _ := os.ReadFile(filepath.Join(e.work, "rel.pub"))
	key := filepath.Join(e.work, "rel.key")
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		manifest(v)
		e.must("!!registry", "publish", pkg, "--registry", reg, "--key", key)
	}
	e.must("!!registry", "revoke", "@pgtools", "v1.1.0", "--registry", reg, "--key", key, "--reason", "CVE-1")

	viper.Set("registry", map[string]any{
		"default": "corp",
		"sources": map[string]any{"corp": map[string]any{"url": "file://" + reg, "signed_by": "rel"}},
		"keys":    map[string]any{"rel": strings.TrimSpace(string(pub))},
	})
	t.Cleanup(func() { viper.Set("registry", nil) })

	out := e.must("!!search", "postgresql")
	for _, want := range []string{"Из corp:", "@pgtools v1.2.0   правила для postgresql.conf", "отозван: CVE-1", "то же, старее"} {
		if !strings.Contains(out, want) {
			t.Fatalf("search: нет %q:\n%s", want, out)
		}
	}
	e.fails("@pgtools v1.1.0 отозван автором (CVE-1) — не ставится; доступна v1.2.0", "!!rin", "@pgtools", "v1.1.0")
	e.fails("не найден в настроенных источниках (corp); других ochan не спрашивает", "!!rin", "@pgtools", "v9")

	out = e.must("!!rin", "@pgtools", "--plan")
	for _, want := range []string{"@pgtools v1.2.0", "подпись:     rel ✓", "режимы:      postgresql.conf", "требует одобрения: да"} {
		if !strings.Contains(out, want) {
			t.Fatalf("план: нет %q:\n%s", want, out)
		}
	}
	out = e.must("!!rin", "@pgtools")
	id := regexpID(t, out)
	e.must("approve", id)

	// Индекс поправили после согласования, не подписав: ему не верят.
	index := filepath.Join(reg, "index.yaml")
	orig, _ := os.ReadFile(index)
	e.write(index, strings.Replace(string(orig), "правила для", "правила  для", 1))
	e.fails("подпись индекса не сходится", "apply", "--request", id)
	e.write(index, string(orig))

	// Архив подменили: сумма не та, что в подписанном индексе.
	archive := filepath.Join(reg, "pgtools-1.2.0.tar.gz")
	good, _ := os.ReadFile(archive)
	old, _ := os.ReadFile(filepath.Join(reg, "pgtools-1.0.0.tar.gz"))
	e.write(archive, string(old))
	e.fails("архив подменён или повреждён", "apply", "--request", id)
	e.write(archive, string(good))

	out = e.must("apply", "--request", id)
	if !strings.Contains(out, "Установлен @pgtools v1.2.0 из corp: sha256") || !strings.Contains(out, "Алиасы: лимит") {
		t.Fatalf("apply:\n%s", out)
	}
	if out := e.must("modules"); !strings.Contains(out, "pgtools") || !strings.Contains(out, "v1.2.0") {
		t.Fatalf("modules:\n%s", out)
	}
	if out := e.must("!!rin", "@pgtools", "--approve"); !strings.Contains(out, "Ставить нечего") {
		t.Fatalf("повторная установка:\n%s", out)
	}

	// --approve — только роли, которая может согласовывать.
	e.must("role", "create", "ops", "--allow", "read,install")
	t.Setenv(EnvRole, "ops")
	e.must("!!install", "--remove", "pgtools")
	e.fails("--approve недоступен", "!!rin", "@pgtools", "--approve")
}

func regexpID(t *testing.T, out string) string {
	t.Helper()
	m := requestIDPattern.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("нет номера заявки:\n%s", out)
	}
	return m[1]
}
