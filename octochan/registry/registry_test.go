package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePackage(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"pg.ochan":     "alias a(c: config) {\n    return c\n}\n",
		"package.yaml": "version: " + version + "\n",
		"notes.txt":    "в архив не идёт",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPackIsReproducibleAndUnpackIsFlat(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pg")
	writePackage(t, dir, "1.0.0")
	a, files, err := Pack(dir)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	b, _, _ := Pack(dir)
	if Sum(a) != Sum(b) || strings.Join(files, ",") != "package.yaml,pg.ochan" {
		t.Fatalf("архив не воспроизводим или не те файлы: %v", files)
	}
	got, err := Unpack(a, filepath.Join(t.TempDir(), "out"))
	if err != nil || len(got) != 2 {
		t.Fatalf("распаковка: %v %v", got, err)
	}

	// Путь с каталогом, ссылка и чужое расширение — отказ.
	for name, hdr := range map[string]*tar.Header{
		"каталог": {Name: "../evil.ochan", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
		"ссылка":  {Name: "x.ochan", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"бинарь":  {Name: "run.sh", Typeflag: tar.TypeReg, Size: 1, Mode: 0o755},
	} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(hdr)
		if hdr.Size > 0 {
			_, _ = tw.Write([]byte("x"))
		}
		_ = tw.Close()
		_ = gz.Close()
		if _, err := Unpack(buf.Bytes(), filepath.Join(t.TempDir(), "out")); err == nil {
			t.Errorf("%s: распакован", name)
		}
	}
}

func TestVersions(t *testing.T) {
	if Compare("1.10", "1.9") != 1 || Compare("1.2", "1.2.0") != 0 || Compare("0.9.1", "1") != -1 {
		t.Fatal("сравнение версий")
	}
	for text, cases := range map[string]map[string]bool{
		"@pgbase >=1.2": {"1.2": true, "1.2.1": true, "1.1.9": false, "2.0": true},
		"@pgbase <2":    {"1.9": true, "2.0.0": false},
		"@pgbase 1.2":   {"1.2.7": true, "1.3": false, "1": false},
		"pgbase":        {"0.1": true},
	} {
		req, err := ParseRequire(text)
		if err != nil {
			t.Fatal(err)
		}
		for have, want := range cases {
			if req.Satisfied(have) != want {
				t.Errorf("%s и %s: %v", text, have, !want)
			}
		}
	}
	if _, err := ParseRequire("@pg base"); err == nil {
		t.Fatal("кривая зависимость разобрана")
	}
	if CleanVersion("v1.4.0") != "1.4.0" || CleanVersion("latest") != "" {
		t.Fatal("CleanVersion")
	}
}

// Источник с signed_by верит только индексу с подписью своего ключа, а
// архиву — только с суммой из этого индекса.
func TestSignedSourceOverHTTP(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	reg := t.TempDir()
	pkg := filepath.Join(t.TempDir(), "pg")
	writePackage(t, pkg, "1.0.0")
	data, files, _ := Pack(pkg)
	if _, err := Publish(reg, "pg", "1.0.0", Meta{Description: "тест"}, data, len(files), priv, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(reg, "pg", "1.0.0", Meta{}, append(data, 0), len(files), priv, time.Now()); err == nil ||
		!strings.Contains(err.Error(), "версия неизменна") {
		t.Fatalf("перепубликация той же версии: %v", err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(reg)))
	defer srv.Close()

	ctx := context.Background()
	client := &Client{Keys: map[string]ed25519.PublicKey{"rel": pub}}
	src := &Source{Name: "corp", URL: srv.URL, Kind: KindHTTP, SignedBy: "rel"}
	f, err := client.Fetch(ctx, src)
	if err != nil || f.SignedBy != "rel" {
		t.Fatalf("подписанный индекс: %v", err)
	}
	ver := f.Index.Packages["pg"].Versions["1.0.0"]
	if got, err := client.Archive(ctx, f, ver); err != nil || Sum(got) != Sum(data) {
		t.Fatalf("архив: %v", err)
	}

	// Чужой ключ, нет ключа, правка индекса — индексу не верят.
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := (&Client{Keys: map[string]ed25519.PublicKey{"rel": other}}).Fetch(ctx, src); err == nil {
		t.Fatal("индекс принят с чужим ключом")
	}
	if _, err := (&Client{}).Fetch(ctx, src); err == nil || !strings.Contains(err.Error(), "нет в registry.keys") {
		t.Fatalf("без ключа: %v", err)
	}
	index, _ := os.ReadFile(filepath.Join(reg, IndexName))
	if err := os.WriteFile(filepath.Join(reg, IndexName), append(index, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Fetch(ctx, src); err == nil || !strings.Contains(err.Error(), "подпись индекса не сходится") {
		t.Fatalf("правленый индекс: %v", err)
	}
	_ = os.WriteFile(filepath.Join(reg, IndexName), index, 0o644)

	// Подменённый архив — не та сумма.
	if err := os.WriteFile(filepath.Join(reg, ver.Archive), []byte("подмена"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Archive(ctx, f, ver); err == nil || !strings.Contains(err.Error(), "архив подменён") {
		t.Fatalf("подменённый архив: %v", err)
	}

	// Отзыв без ключа снимает прежнюю подпись: она к новому индексу не подходит.
	if err := Revoke(reg, "pg", "1.0.0", "CVE", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(reg, IndexSig)); !os.IsNotExist(err) {
		t.Fatal("подпись осталась у индекса, который ею не подписан")
	}
}

func TestIndexRejectsArchiveOutside(t *testing.T) {
	for _, archive := range []string{"../x.tar.gz", "/etc/passwd", `..\x`} {
		text := "packages:\n  pg:\n    versions:\n      1.0.0: {archive: '" + archive + "', sha256: ab}\n"
		if _, err := ParseIndex([]byte(text)); err == nil {
			t.Errorf("%s принят", archive)
		}
	}
	if _, err := ParseIndex([]byte("protocol: 9\npackages: {}\n")); err == nil || !strings.Contains(err.Error(), "обновите ochan") {
		t.Fatalf("протокол новее: %v", err)
	}
}
