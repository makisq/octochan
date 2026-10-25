package audit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Ребёнок для теста параллельной записи разными процессами.
	if path := os.Getenv("AUDIT_TEST_WRITER"); path != "" {
		j := &Journal{Path: path}
		for i := 0; i < 20; i++ {
			if err := j.Append(Record{Event: EventCommand, Role: "child", Command: strconv.Itoa(i)}, nil); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func journal(t *testing.T) *Journal {
	t.Helper()
	return &Journal{Path: filepath.Join(t.TempDir(), "audit.log")}
}

func fill(t *testing.T, j *Journal, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := j.Append(Record{Event: EventCommand, Role: "dan", Command: "diff", Args: []string{strconv.Itoa(i)}}, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func lines(t *testing.T, j *Journal) [][]byte {
	t.Helper()
	data, err := os.ReadFile(j.Path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.SplitAfter(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
}

func rewrite(t *testing.T, j *Journal, ls [][]byte) {
	t.Helper()
	var out []byte
	for _, l := range ls {
		out = append(out, l...)
		if !bytes.HasSuffix(l, []byte{'\n'}) {
			out = append(out, '\n')
		}
	}
	if err := os.WriteFile(j.Path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestChainVerifies(t *testing.T) {
	j := journal(t)
	fill(t, j, 5)
	summary, err := j.Verify("")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records != 5 || len(summary.Head) != 64 {
		t.Fatalf("итог: %+v", summary)
	}
	if _, err := j.Verify(summary.Head[:12]); err != nil {
		t.Fatalf("сохранённый хеш последней записи не нашёлся: %v", err)
	}
	last, err := j.last()
	if err != nil || last.Seq != 5 || last.Hash != summary.Head {
		t.Fatalf("последняя запись: %+v, %v", last, err)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	cases := map[string]func([][]byte) [][]byte{
		"правка": func(ls [][]byte) [][]byte {
			ls[2] = bytes.Replace(ls[2], []byte(`"role":"dan"`), []byte(`"role":"eve"`), 1)
			return ls
		},
		"удаление": func(ls [][]byte) [][]byte { return append(ls[:2], ls[3:]...) },
		"перестановка": func(ls [][]byte) [][]byte {
			ls[1], ls[2] = ls[2], ls[1]
			return ls
		},
		"подмена хеша": func(ls [][]byte) [][]byte {
			// Правка вместе с пересчётом собственного хеша записи всё
			// равно рвёт цепочку: следующая ссылается на старый хеш.
			rec, _ := decode(bytes.TrimSuffix(ls[2], []byte{'\n'}))
			rec.Role = "eve"
			line, _, _ := encode(rec)
			ls[2] = line
			return ls
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			j := journal(t)
			fill(t, j, 5)
			rewrite(t, j, tamper(lines(t, j)))
			if _, err := j.Verify(""); err == nil {
				t.Fatal("правка журнала не замечена")
			}
		})
	}
}

func TestTruncationNeedsSavedHead(t *testing.T) {
	j := journal(t)
	fill(t, j, 5)
	summary, _ := j.Verify("")
	rewrite(t, j, lines(t, j)[:3])
	if _, err := j.Verify(""); err != nil {
		t.Fatalf("обрезанный хвост без сохранённого хеша не виден, а проверка упала: %v", err)
	}
	if _, err := j.Verify(summary.Head); err == nil || !strings.Contains(err.Error(), "обрезан") {
		t.Fatalf("обрезка не замечена по сохранённому хешу: %v", err)
	}
}

func TestGuardRefusalIsRecorded(t *testing.T) {
	j := journal(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	j.Now = func() time.Time { return now }

	limit := func(recent Recent) error {
		records, err := recent(now.Add(-time.Hour))
		if err != nil {
			return err
		}
		allowed := 0
		for _, r := range records {
			if r.Allowed {
				allowed++
			}
		}
		if allowed >= 2 {
			return errors.New("предел исчерпан")
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		err := j.Append(Record{Event: EventCommand, Role: "dan", Command: "apply"}, limit)
		if (i < 2) != (err == nil) {
			t.Fatalf("попытка %d: %v", i+1, err)
		}
		now = now.Add(time.Minute)
	}
	records, err := j.Last(0, nil)
	if err != nil || len(records) != 3 {
		t.Fatalf("записи: %v, %v", records, err)
	}
	if records[2].Allowed || records[2].Reason != "предел исчерпан" {
		t.Fatalf("отказ не записан: %+v", records[2])
	}

	// Окно сдвинулось: старые записи за пределами часа не считаются.
	now = now.Add(2 * time.Hour)
	if err := j.Append(Record{Event: EventCommand, Role: "dan", Command: "apply"}, limit); err != nil {
		t.Fatalf("через два часа предел должен был освободиться: %v", err)
	}
}

func TestTailAcrossBlocks(t *testing.T) {
	j := journal(t)
	long := strings.Repeat("я", 20_000) // запись длиннее половины блока
	for i := 0; i < 12; i++ {
		if err := j.Append(Record{Event: EventCommand, Role: "dan", Args: []string{long, strconv.Itoa(i)}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	records, err := j.Last(0, nil)
	if err != nil || len(records) != 12 {
		t.Fatalf("прочитано %d записей: %v", len(records), err)
	}
	for i, rec := range records {
		if rec.Seq != int64(i+1) || rec.Args[1] != strconv.Itoa(i) {
			t.Fatalf("запись %d: seq %d, args[1] %q", i, rec.Seq, rec.Args[1])
		}
	}
	picked, err := j.Last(2, func(r Record) bool { return r.Args[1] == "3" || r.Args[1] == "7" || r.Args[1] == "9" })
	if err != nil || len(picked) != 2 || picked[0].Args[1] != "7" || picked[1].Args[1] != "9" {
		t.Fatalf("отбор: %v %v", picked, err)
	}
}

func TestConcurrentWritersKeepChain(t *testing.T) {
	j := journal(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := exec.Command(exe)
			c.Env = append(os.Environ(), "AUDIT_TEST_WRITER="+j.Path)
			if out, err := c.CombinedOutput(); err != nil {
				t.Errorf("процесс: %v\n%s", err, out)
			}
		}()
	}
	fill(t, j, 20)
	wg.Wait()

	summary, err := j.Verify("")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records != 100 {
		t.Fatalf("записей %d, ожидалось 100", summary.Records)
	}
}
