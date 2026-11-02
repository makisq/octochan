package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ochan/embedded"
)

// fakeAdapter — тестовый бинарь в роли adapter.py: ochan запускает его как
// «python adapter.py serve --model … --listen …».
func fakeAdapter() int {
	if os.Getenv("OCHAN_TEST_ADAPTER_CRASH") == "1" {
		return 3
	}
	var model, listen string
	for i, a := range os.Args {
		switch {
		case a == "--model" && i+1 < len(os.Args):
			model = os.Args[i+1]
		case a == "--listen" && i+1 < len(os.Args):
			listen = os.Args[i+1]
		}
	}
	h := embedded.Handler(embedded.Info{Engine: "fake", Model: model}, &embedded.Demo{Answer: formalNote})
	if err := http.ListenAndServe(listen, h); err != nil {
		return 1
	}
	return 0
}

// adapterEnv — модель, «adapter.py» и «python» для ochan llm serve.
func adapterEnv(t *testing.T, e *scriptEnv) (model, addr string) {
	t.Helper()
	model = filepath.Join(e.home, "models", "own-tiny")
	if err := os.MkdirAll(model, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(e.home, "llm", "adapter.py")
	_ = os.MkdirAll(filepath.Dir(script), 0o700)
	e.write(script, "# поддельный адаптер\n")
	addr = freePort(t)
	t.Setenv("OCHAN_TEST_FAKE_ADAPTER", "1")
	for k, v := range map[string]any{"llm.mode": "embedded", "llm.endpoint": "http://" + addr, "llm.model": model,
		"llm.adapter.script": script, "llm.adapter.python": os.Args[0], "llm.adapter.start_timeout": "20s"} {
		setting(t, k, v)
	}
	return model, addr
}

// ochan запускает адаптер своей модели сам: llm.model доходит до него, а
// упавший перезапускается, пока не исчерпаны попытки.
func TestLLMAdapterManaged(t *testing.T) {
	e := newScriptEnv(t)
	model, addr := adapterEnv(t, e)

	// Не отвечает и autostart выключен — подсказка, а не молчание.
	if out, err := e.ochan("llm"); err == nil || !strings.Contains(out, "Запуск:  ochan llm serve") {
		t.Fatalf("llm без адаптера: %v\n%s", err, out)
	}

	// autostart: generate сам запускает адаптер, отвечает и гасит его.
	setting(t, "llm.adapter.autostart", true)
	out := e.must("llm", "generate", "--out", e.path("answer.fp"))
	if !strings.Contains(out, "Адаптер не отвечает — запускаю") || e.read("answer.fp") != formalNote {
		t.Fatalf("generate с автозапуском:\n%s", out)
	}
	if _, err := http.Get("http://" + addr + "/v1/info"); err == nil {
		t.Fatal("адаптер, запущенный generate, остался жив")
	}

	// serve: та модель, что в llm.model; остановка — по ctx.
	spec, err := adapterConfig()
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := llmEngine()
	ctx, cancel := context.WithCancel(context.Background())
	var lines []string
	done := make(chan error, 1)
	go func() {
		done <- superviseAdapter(ctx, spec, engine, io.Discard, func(f string, a ...any) {
			lines = append(lines, strings.TrimSpace(fmt.Sprintf(f, a...)))
			if strings.HasPrefix(f, "Адаптер готов") {
				cancel()
			}
		})
	}()
	select {
	case err := <-done:
		if err != nil || len(lines) == 0 || !strings.Contains(lines[0], "модель "+model) {
			t.Fatalf("serve: %v %v", err, lines)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve не остановился")
	}

	// Другая модель по адресу — внимание в ochan llm.
	setting(t, "llm.adapter.autostart", false)
	other := filepath.Join(e.home, "models", "own-other")
	_ = os.MkdirAll(other, 0o700)
	running, stopRunning := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		_ = superviseAdapter(running, spec, engine, io.Discard, func(string, ...any) {})
		close(stopped)
	}()
	t.Cleanup(func() { stopRunning(); <-stopped })
	if _, err := waitAdapter(context.Background(), engine, &adapterProc{done: make(chan struct{})}, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	setting(t, "llm.model", other)
	if out := e.must("llm"); !strings.Contains(out, "внимание: по адресу отвечает модель "+model+", а llm.model — "+other) {
		t.Fatalf("другая модель:\n%s", out)
	}
}

// Адаптер падает сразу — перезапуски с паузой, потом ochan сдаётся.
func TestLLMAdapterCrashes(t *testing.T) {
	e := newScriptEnv(t)
	adapterEnv(t, e)
	t.Setenv("OCHAN_TEST_ADAPTER_CRASH", "1")
	setting(t, "llm.adapter.restarts", 1)
	spec, err := adapterConfig()
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := llmEngine()
	var lines []string
	err = superviseAdapter(context.Background(), spec, engine, io.Discard, func(f string, a ...any) {
		lines = append(lines, fmt.Sprintf(f, a...))
	})
	if err == nil || !strings.Contains(err.Error(), "адаптер падает 2 раз подряд") || len(lines) != 1 ||
		!strings.Contains(lines[0], "перезапуск 1 из 1") {
		t.Fatalf("падающий адаптер: %v %v", err, lines)
	}
	setting(t, "llm.model", "")
	if _, err := adapterConfig(); err == nil || !strings.Contains(err.Error(), "llm.model: каталог своей модели") {
		t.Fatalf("без llm.model: %v", err)
	}

}
