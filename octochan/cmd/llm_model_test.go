package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ochan/embedded"

	"github.com/spf13/viper"
)

// Своя модель целиком в ochan: init (словарь и сеть), generate в процессе,
// fit по журналу, версии и откат, корпус с токенами, квантование со
// сверкой.
func TestLLMOwnModelInProcess(t *testing.T) {
	e := newScriptEnv(t)
	dir := filepath.Join(e.home, "models", "own")
	out := e.must("llm", "init", "--out", dir, "--size", "tiny", "--random-weights")
	if !strings.Contains(out, "Модель "+dir+": словарь") || !strings.Contains(out, "Веса случайные") {
		t.Fatalf("init:\n%s", out)
	}
	e.fails("уже модель", "llm", "init", "--out", dir)
	setting(t, "llm.mode", "embedded")
	setting(t, "llm.model", dir)
	t.Cleanup(func() {
		loadedMu.Lock()
		delete(loaded, dir)
		loadedMu.Unlock()
	})

	out = e.must("llm")
	if !strings.Contains(out, "Модель:  "+dir+": 5.3 млн параметров, словарь") || !strings.Contains(out, "в процессе ochan") ||
		!strings.Contains(out, "Подход:  архитектурный") {
		t.Fatalf("llm:\n%s", out)
	}

	e.write(e.path("task.fp"), loopTask)
	out = e.must("llm", "generate", "--prompt", e.path("task.fp"))
	if !strings.Contains(out, "Архитектурная модель: ответ написала она сама") || !strings.Contains(out, "set Approval.limit = 200") ||
		!strings.Contains(out, "Журнал решений: запись") {
		t.Fatalf("generate:\n%s", out)
	}

	out = e.must("llm", "fit")
	if !strings.Contains(out, "Версия параметров 1") || !strings.Contains(out, "Веса и калибровка прежние") {
		t.Fatalf("fit:\n%s", out)
	}
	e.must("llm", "fit")
	if out := e.must("llm", "learn", "--versions"); !strings.Contains(out, "журнал решений: журнал этой машины") {
		t.Fatalf("версии:\n%s", out)
	}
	if out := e.must("llm", "learn", "--rollback"); !strings.Contains(out, "Параметры откачены к версии 1: это версия 3") {
		t.Fatalf("откат:\n%s", out)
	}

	corpus := e.path("corpus.jsonl")
	out = e.must("llm", "corpus", "--count", "30", "--out", corpus)
	if !strings.Contains(out, "Токены — словарём модели") {
		t.Fatalf("corpus:\n%s", out)
	}
	f, _ := os.Open(corpus)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	lines := 0
	for sc.Scan() {
		var p struct {
			IDs   []int  `json:"ids"`
			Start int    `json:"start"`
			Split string `json:"split"`
		}
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil || len(p.IDs) == 0 || p.Start == 0 || p.Split == "" {
			t.Fatalf("пара корпуса: %v %s", err, sc.Text())
		}
		lines++
	}
	f.Close()
	if lines < 30 {
		t.Fatalf("пар %d", lines)
	}

	q := filepath.Join(e.home, "models", "own-int8")
	out = e.must("llm", "quantize", "--bits", "8", "--out", q, "--check", corpus, "--pairs", "1")
	if !strings.Contains(out, "int8, группа строка: веса") || !strings.Contains(out, "Сверка на 1 отложенных парах") {
		t.Fatalf("quantize:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(q, "rings.json")); err != nil {
		t.Fatalf("параметры колец не перешли с весами: %v", err)
	}

	// Не своя модель в llm.model — отказ с подсказкой.
	setting(t, "llm.model", e.work)
	e.fails("не своя модель ℒ", "llm", "generate", "--prompt", e.path("task.fp"))
}

// ochan llm serve выставляет свою модель по протоколу v1: ochan другой
// машины говорит с ней по адресу. Не локальный адрес — только с
// --allow-remote.
func TestLLMServeOwnModel(t *testing.T) {
	e := newScriptEnv(t)
	dir := filepath.Join(e.home, "models", "own")
	e.must("llm", "init", "--out", dir, "--size", "tiny", "--random-weights")
	addr := freePort(t)
	for k, v := range map[string]any{"llm.mode": "embedded", "llm.model": dir, "llm.endpoint": "http://" + addr} {
		viper.Set(k, v)
	}
	t.Cleanup(func() {
		viper.Set("llm.mode", "")
		viper.Set("llm.model", "")
		viper.Set("llm.endpoint", "http://127.0.0.1:8080")
	})
	if err := viper.WriteConfigAs(os.Getenv("OCTOCHAN_CONFIG")); err != nil {
		t.Fatal(err)
	}
	if _, err := serveAddress("http://10.1.2.3:8093", false); err == nil || !strings.Contains(err.Error(), "--allow-remote") {
		t.Fatalf("не локальный адрес: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	self, _ := os.Executable()
	c := exec.CommandContext(ctx, self, "llm", "serve")
	c.Env = append(os.Environ(), "OCHAN_TEST_AS_OCHAN=1")
	stdout, _ := c.StdoutPipe()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = c.Wait() })
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "Своя модель:") {
				ready <- sc.Text()
			}
		}
	}()
	select {
	case line := <-ready:
		if !strings.Contains(line, "протокол v1") || !strings.Contains(line, dir) {
			t.Fatalf("serve: %s", line)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("сервер модели не поднялся")
	}

	client := embedded.NewHTTP("http://"+addr, 30*time.Second)
	info, err := client.Info(ctx)
	if err != nil || !info.Generate || info.Model != dir {
		t.Fatalf("info %+v: %v", info, err)
	}
	ans, err := client.Generate(ctx, "protocol\n\n# Task\n\n"+loopTask)
	if err != nil || ans.Refused || !strings.Contains(ans.Text, "set Approval.limit = 200") {
		t.Fatalf("generate: %v %+v", err, ans)
	}
}
