package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"ochan/embedded"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Адаптер своей модели запускает сам ochan.
//
//	llm:
//	  endpoint: http://127.0.0.1:8093
//	  model: G:/fpm-llm/models/own-small-2   # каталог своей модели: его получает адаптер
//	  adapter:
//	    python: G:/fpm-llm/venv/Scripts/python   # пусто — venv рядом с adapter.py, иначе python3
//	    script: ""                               # пусто — adapter.py, который поставил make install-llm
//	    device: ""                               # cuda | cpu; пусто — выберет адаптер
//	    autostart: true                          # llm generate и llm run запускают его сами
//	    restarts: 5                              # перезапусков упавшего подряд
//	    start_timeout: 180s                      # сколько ждать, пока модель загрузится
//
// Адаптер слушает адрес из llm.endpoint, читает пакеты знаний из каталога
// модулей ochan. Его вывод — в журнал ~/.octochan/logs/adapter.log.

type adapterSpec struct {
	python, script, model, listen, modules, device string
	restarts                                       int
	startTimeout                                   time.Duration
}

// adapterConfig — как запускать адаптер; ошибка говорит, чего не хватает.
func adapterConfig() (adapterSpec, error) {
	s := adapterSpec{
		model:        expandHome(strings.TrimSpace(viper.GetString("llm.model"))),
		device:       strings.TrimSpace(viper.GetString("llm.adapter.device")),
		restarts:     viper.GetInt("llm.adapter.restarts"),
		startTimeout: viper.GetDuration("llm.adapter.start_timeout"),
		modules:      modulesDir(),
	}
	if s.model == "" {
		return s, errors.New("llm.model: каталог своей модели — без него адаптеру нечего загружать")
	}
	if _, err := os.Stat(s.model); err != nil {
		return s, fmt.Errorf("llm.model: %v", err)
	}
	endpoint := strings.TrimSpace(viper.GetString("llm.endpoint"))
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Port() == "" {
		return s, fmt.Errorf("llm.endpoint: %q — адаптер слушает http://адрес:порт на этой машине (TLS у него нет)", endpoint)
	}
	s.listen = u.Host
	if s.script, err = adapterScript(); err != nil {
		return s, err
	}
	if s.python, err = adapterPython(s.script); err != nil {
		return s, err
	}
	if s.restarts < 0 {
		s.restarts = 0
	}
	if s.startTimeout <= 0 {
		s.startTimeout = 180 * time.Second
	}
	return s, nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~"); ok && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// adapterScript — adapter.py: из llm.adapter.script, иначе там, куда его
// кладёт установка (make install-llm, install.ps1 -Llm).
func adapterScript() (string, error) {
	if s := expandHome(strings.TrimSpace(viper.GetString("llm.adapter.script"))); s != "" {
		if _, err := os.Stat(s); err != nil {
			return "", fmt.Errorf("llm.adapter.script: %v", err)
		}
		return s, nil
	}
	var tried []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, c := range []string{
			filepath.Join(dir, "..", "share", "ochan", "llm", "adapter.py"),
			filepath.Join(dir, "llm", "adapter.py"),
		} {
			if _, err := os.Stat(c); err == nil {
				return filepath.Clean(c), nil
			}
			tried = append(tried, filepath.Clean(c))
		}
	}
	return "", fmt.Errorf("адаптер не найден (%s): поставьте его (make install-llm, install.ps1 -Llm) "+
		"или укажите llm.adapter.script", strings.Join(tried, ", "))
}

// adapterPython — интерпретатор: llm.adapter.python, venv рядом с
// adapter.py, иначе python3 (python на Windows).
func adapterPython(script string) (string, error) {
	if p := expandHome(strings.TrimSpace(viper.GetString("llm.adapter.python"))); p != "" {
		return p, nil
	}
	venv := filepath.Join(filepath.Dir(script), "venv", "bin", "python")
	if runtime.GOOS == "windows" {
		venv = filepath.Join(filepath.Dir(script), "venv", "Scripts", "python.exe")
	}
	if _, err := os.Stat(venv); err == nil {
		return venv, nil
	}
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("llm.adapter.python: интерпретатор Python с llama/requirements.txt не найден")
}

func (s adapterSpec) args() []string {
	args := []string{s.script, "serve", "--model", s.model, "--listen", s.listen, "--modules", s.modules}
	if s.device != "" {
		args = append(args, "--device", s.device)
	}
	return args
}

func (s adapterSpec) describe() string {
	return s.python + " " + strings.Join(s.args(), " ")
}

// adapterLog — журнал адаптера: дописывается.
func adapterLog() (*os.File, string, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, "", err
	}
	dir := viper.GetString("defaults.log_path")
	if dir == "" {
		dir = fm.LogsDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "adapter.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	return f, path, err
}

// adapterProc — запущенный адаптер.
type adapterProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startAdapter(s adapterSpec, out io.Writer) (*adapterProc, error) {
	c := exec.Command(s.python, s.args()...)
	c.Stdout, c.Stderr = out, out
	c.Dir = filepath.Dir(s.script)
	// Вывод Python — без буфера: строки журнала приходят сразу.
	c.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8")
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("адаптер не запустился (%s): %w", s.describe(), err)
	}
	p := &adapterProc{cmd: c, done: make(chan struct{})}
	go func() {
		p.err = c.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *adapterProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// stop останавливает адаптер: сначала просьбой, через 5 секунд — силой.
func (p *adapterProc) stop() {
	if p.exited() {
		return
	}
	if runtime.GOOS == "windows" {
		_ = p.cmd.Process.Kill()
	} else {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// waitAdapter ждёт, пока адаптер ответит на /v1/info: модель загружена.
func waitAdapter(ctx context.Context, engine *embedded.HTTP, p *adapterProc, timeout time.Duration) (embedded.Info, error) {
	deadline := time.Now().Add(timeout)
	for {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		info, err := engine.Info(probe)
		cancel()
		if err == nil {
			return info, nil
		}
		if p.exited() {
			return embedded.Info{}, fmt.Errorf("адаптер завершился, не ответив (%v)", p.err)
		}
		if time.Now().After(deadline) {
			return embedded.Info{}, fmt.Errorf("адаптер не ответил за %s: %v", timeout, err)
		}
		select {
		case <-ctx.Done():
			return embedded.Info{}, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// checkModelPath — по адресу отвечает модель из llm.model.
func checkModelPath(info embedded.Info) string {
	want := expandHome(strings.TrimSpace(viper.GetString("llm.model")))
	if want == "" || info.Model == "" {
		return ""
	}
	if sameModel(want, info.Model) {
		return ""
	}
	return fmt.Sprintf("по адресу отвечает модель %s, а llm.model — %s", info.Model, want)
}

func sameModel(a, b string) bool {
	clean := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return strings.TrimRight(filepath.ToSlash(filepath.Clean(p)), "/")
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(clean(a), clean(b))
	}
	return clean(a) == clean(b)
}

// superviseAdapter держит адаптер запущенным, пока жив ctx: упал —
// перезапуск с паузой, которая растёт; упал restarts раз подряд, не
// проработав минуты, — ошибка.
func superviseAdapter(ctx context.Context, s adapterSpec, engine *embedded.HTTP, out io.Writer,
	report func(format string, args ...any)) error {
	failures := 0
	pause := time.Second
	for {
		started := time.Now()
		p, err := startAdapter(s, out)
		if err != nil {
			return err
		}
		info, err := waitAdapter(ctx, engine, p, s.startTimeout)
		if err != nil && !p.exited() {
			// Жив, но не отвечает: такой адаптер — не адаптер.
			p.stop()
		}
		if err == nil {
			report("Адаптер готов: %s — модель %s, pid %d\n", engine.Endpoint, info.Model, p.cmd.Process.Pid)
			if warn := checkModelPath(info); warn != "" {
				report("внимание: %s\n", warn)
			}
		}
		select {
		case <-ctx.Done():
			p.stop()
			report("Адаптер остановлен\n")
			return nil
		case <-p.done:
		}
		if time.Since(started) > time.Minute {
			failures, pause = 0, time.Second
		}
		failures++
		if err == nil {
			err = p.err
		}
		if failures > s.restarts {
			return fmt.Errorf("адаптер падает %d раз подряд: %v — журнал адаптера покажет почему", failures, err)
		}
		report("Адаптер завершился (%v); перезапуск %d из %d через %s\n", err, failures, s.restarts, pause)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pause):
		}
		if pause < 30*time.Second {
			pause *= 2
		}
	}
}

// ensureAdapter — движок отвечает, а нет — с llm.adapter.autostart ochan
// запускает адаптер сам и держит его, пока работает команда. stop
// останавливает то, что запущено здесь.
func ensureAdapter(cmd *cobra.Command, engine *embedded.HTTP) (stop func(), err error) {
	probe, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
	_, err = engine.Info(probe)
	cancel()
	if err == nil || !viper.GetBool("llm.adapter.autostart") {
		return func() {}, nil
	}
	spec, err := adapterConfig()
	if err != nil {
		return nil, fmt.Errorf("адаптер не отвечает, а запустить его нечем: %w", err)
	}
	log, path, err := adapterLog()
	if err != nil {
		return nil, err
	}
	ctx, done := context.WithCancel(context.Background())
	var once sync.Once
	finished := make(chan error, 1)
	ready := make(chan struct{})
	cmd.PrintErrf("Адаптер не отвечает — запускаю: %s (журнал: %s)\n", spec.describe(), path)
	go func() {
		finished <- superviseAdapter(ctx, spec, engine, log, func(format string, args ...any) {
			fmt.Fprintf(log, format, args...)
			if strings.HasPrefix(format, "Адаптер готов") {
				once.Do(func() { close(ready) })
			}
		})
	}()
	stop = func() {
		done()
		<-finished
		_ = log.Close()
	}
	select {
	case <-ready:
		return stop, nil
	case err := <-finished:
		done()
		_ = log.Close()
		if err == nil {
			err = errors.New("адаптер остановился")
		}
		return nil, err
	case <-time.After(spec.startTimeout):
		stop()
		return nil, fmt.Errorf("адаптер не ответил за %s — журнал: %s", spec.startTimeout, path)
	}
}

var llmServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Запустить адаптер своей модели и держать его: упал — перезапуск",
	Long: "Запускает adapter.py serve с моделью llm.model на адресе llm.endpoint;\n" +
		"пакеты знаний адаптер читает из каталога модулей ochan. Ждёт, пока модель\n" +
		"загрузится и ответит на /v1/info, и сверяет, что отвечает та модель, что в\n" +
		"llm.model. Упавший адаптер перезапускается с растущей паузой; упал\n" +
		"llm.adapter.restarts раз подряд, не проработав минуты, — ochan сдаётся.\n" +
		"Остановить — Ctrl+C. Вывод адаптера — здесь и в журнале adapter.log.\n\n" +
		"Настройки — llm.model, llm.endpoint и llm.adapter.* в config.yaml.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		spec, err := adapterConfig()
		if err != nil {
			return err
		}
		engine, err := llmEngine()
		if err != nil {
			return err
		}
		probe, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		info, err := engine.Info(probe)
		cancel()
		if err == nil {
			return fmt.Errorf("по адресу %s уже отвечает модель %s — второй адаптер на том же адресе не запустить", engine.Endpoint, info.Model)
		}
		log, path, err := adapterLog()
		if err != nil {
			return err
		}
		defer log.Close()
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		out := io.MultiWriter(cmd.OutOrStdout(), log)
		cmd.Printf("Адаптер: %s\nЖурнал:  %s\n", spec.describe(), path)
		return superviseAdapter(ctx, spec, engine, out, func(format string, args ...any) {
			fmt.Fprintf(out, format, args...)
		})
	},
}

func init() {
	llmCmd.AddCommand(llmServeCmd)
}
