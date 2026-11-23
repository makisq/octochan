package own

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pretrainedTask = `goal: backup

classes:
  Backup : "backup"
  Schedule : "schedule"
  Database : "database"

relations:
  Schedule --defines--> Database (nightly_backup)
  Database --strives_to--> Backup

cell c1:
  set Schedule.period = daily
`

// Встроенные файлы — те самые, чьи суммы названы в описании весов: подмена
// или порча файла в репозитории не проходит молча.
func TestPretrainedChecksums(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("pretrained", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	sums := map[string]string{}
	for _, line := range strings.Split(string(readme), "\n") {
		if f := strings.Fields(line); len(f) == 2 && len(f[0]) == 64 {
			sums[f[1]] = f[0]
		}
	}
	for _, name := range pretrainedFiles {
		data, err := pretrained.ReadFile("pretrained/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != sums[name] {
			t.Errorf("%s: сумма %s, в описании %q", name, got, sums[name])
		}
		if strings.HasSuffix(name, ".json") && strings.ContainsRune(string(data), '\r') {
			t.Errorf("%s: в тексте возврат каретки — git сохранит файл иначе, чем он встроен", name)
		}
	}
}

// Стартовая модель из бинарника — рабочая: загружается и отвечает; полные
// веса (full) дают ту же сеть, без служебных тензоров квантования.
func TestWritePretrained(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "model")
	if err := WritePretrained(dir, false); err != nil {
		t.Fatal(err)
	}
	if bits := QuantBits(dir); bits != 8 {
		t.Fatalf("веса стартовой модели: int%d", bits)
	}
	m, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Tok.Len() != 1682 || m.Net.Params() < 14_000_000 {
		t.Fatalf("стартовая модель: словарь %d, параметров %d", m.Tok.Len(), m.Net.Params())
	}
	ans, err := m.Generate(context.Background(), TaskMarker+pretrainedTask)
	if err != nil || ans.Refused || !strings.Contains(ans.Text, "set Database.period = daily") ||
		!strings.Contains(ans.Text, "basis: relation(nightly_backup)") {
		t.Fatalf("ответ стартовой модели: %v\n%+v", err, ans)
	}
	if err := WritePretrained(dir, false); err == nil {
		t.Fatal("стартовая модель записана поверх готовой")
	}

	full := filepath.Join(t.TempDir(), "full")
	if err := WritePretrained(full, true); err != nil {
		t.Fatal(err)
	}
	if IsQuantized(full) {
		t.Fatal("в полной модели остались квантованные веса")
	}
	tensors, err := ReadSafetensors(filepath.Join(full, "model.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	for name := range tensors {
		if strings.HasSuffix(name, ".scale") || strings.HasSuffix(name, ".q") {
			t.Fatalf("в полных весах служебный тензор %s", name)
		}
	}
	other, err := LoadNet(full)
	if err != nil {
		t.Fatal(err)
	}
	ids, _, _, err := m.Tok.Pair(pretrainedTask, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.Net.Probs(ids)
	if err != nil {
		t.Fatal(err)
	}
	b, err := other.Probs(ids)
	if err != nil {
		t.Fatal(err)
	}
	for i := range a {
		if d := a[i] - b[i]; d > 1e-6 || d < -1e-6 {
			t.Fatalf("полные веса — другая сеть: p[%d] %g против %g", i, a[i], b[i])
		}
	}
	if _, err := os.Stat(filepath.Join(full, quantWeights)); err == nil {
		t.Fatal("квантованный файл не убран")
	}
}
