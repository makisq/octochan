package cmd

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"octochan/i18n"
	"octochan/lang"
	"octochan/sandbox"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Сценарий в песочнице выполняется отдельным процессом ochan под
// ограничениями ядра (пакет sandbox). Права проверяет родитель — до
// запуска ребёнка; ребёнок ролей не знает и не читает: ни хранилища ролей,
// ни конфига, ни журнала ему не видно.
//
// Задание ребёнок получает одним JSON на stdin, а отвечает кадрами в
// stdout: вывод сценария, его ошибки и в конце итог — действия, вызовы
// алиасов, ошибка. Кадр — вид, длина и данные, поэтому сценарий не
// подделает итог печатью: всё, что он печатает, — данные кадра вывода.

// isolatedSpec — задание процессу песочницы.
type isolatedSpec struct {
	// Mode — plan, run, call (вызов алиаса с аргументами) или probe
	// (пустой запуск: проверить, что песочница ставится).
	Mode    string   `json:"mode"`
	Name    string   `json:"name,omitempty"`
	Content string   `json:"content,omitempty"`
	Alias   string   `json:"alias,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Check — проверить сценарий целиком до выполнения.
	Check bool `json:"check,omitempty"`
	// Dir — каталог для относительных путей; пусто — текущий.
	Dir   string   `json:"dir,omitempty"`
	Roots []string `json:"roots"`
	Env   []string `json:"env,omitempty"`
	Live  []string `json:"live,omitempty"`
	// Protected — каталоги, закрытые для встроенных функций.
	Protected []string `json:"protected,omitempty"`

	AliasesDir  string `json:"aliases_dir"`
	ModulesDir  string `json:"modules_dir"`
	MaxParallel int    `json:"max_parallel"`
	Lang        string `json:"lang,omitempty"`
}

// isolatedResult — итог процесса песочницы.
type isolatedResult struct {
	Effects     []lang.Effect     `json:"effects,omitempty"`
	Invocations []lang.Invocation `json:"invocations,omitempty"`
	// Value — результат алиаса текстом; HasValue — он не null.
	Value    string `json:"value,omitempty"`
	HasValue bool   `json:"has_value,omitempty"`
	Error    string `json:"error,omitempty"`

	// report — какая изоляция действовала; заполняет родитель.
	report sandbox.Report
}

func (r *isolatedResult) err() error {
	if r.Error == "" {
		return nil
	}
	return errors.New(r.Error)
}

// Виды кадров.
const (
	frameOut    = 'o'
	frameErr    = 'e'
	frameResult = 'r'
)

// maxFrame — предел кадра. Итог несёт содержимое файлов до и после
// записи, а журнал отката хранит не больше maxRollbackBytes: вчетверо
// больше — с запасом.
const maxFrame = 4*maxRollbackBytes + 1<<20

// setBox переносит в задание песочницу языка.
func (s *isolatedSpec) setBox(box *lang.Sandbox) {
	s.Roots, s.Env, s.Live = box.Roots(), box.Env(), box.Live()
}

// isolated выполняет задание в процессе песочницы.
func isolated(cmd *cobra.Command, spec isolatedSpec, out io.Writer) (*isolatedResult, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	spec.AliasesDir, spec.ModulesDir = fm.AliasesDir(), fm.ModulesDir()
	spec.MaxParallel = viper.GetInt("defaults.max_parallel_tasks")
	spec.Lang = i18n.Current()
	spec.Protected = protectedPaths()

	policy := sandbox.Policy{
		Write:   spec.Roots,
		Read:    []string{spec.AliasesDir, spec.ModulesDir},
		Network: len(spec.Live) > 0,
	}
	probe := spec.Mode == "probe"
	if report := probeSandbox(policy); !probe && viper.GetBool("sandbox.strict") && !report.Full() {
		return nil, i18n.Errorf("песочница неполная, а sandbox.strict требует полной:\n  %s",
			strings.Join(report.Gaps(), "\n  "))
	}

	// Окружение ребёнка — только разрешённые переменные: остальное, вместе
	// с токенами и паролями, он не получает вовсе.
	var env []string
	for _, name := range spec.Env {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}

	input, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	frames := &frameReader{out: out, errOut: cmd.ErrOrStderr()}
	stderr := &cappedBuffer{limit: 64 << 10}
	dir := spec.Dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("не удалось определить текущий каталог: %w", err)
		}
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	child := &sandbox.Command{Policy: policy, Env: env, Dir: dir,
		Stdin: bytes.NewReader(input), Stdout: frames, Stderr: stderr}
	proc, err := child.Start(ctx)
	if err != nil {
		return nil, err
	}
	if !probe {
		warnIsolation(cmd, proc.Report)
	}
	waitErr := proc.Wait()

	switch {
	case frames.err != nil:
		return nil, fmt.Errorf("процесс песочницы ответил не по протоколу: %w", frames.err)
	case frames.result != nil:
		frames.result.report = proc.Report
		return frames.result, nil
	case sandbox.ExitFailed(waitErr):
		return nil, fmt.Errorf("ограничения песочницы не поставлены, сценарий не выполнялся: %s",
			strings.TrimSpace(stderr.String()))
	case ctx.Err() != nil:
		return nil, fmt.Errorf("сценарий в песочнице прерван: %w", ctx.Err())
	default:
		return nil, fmt.Errorf("процесс песочницы завершился без итога (%v): %s",
			waitErr, strings.TrimSpace(stderr.String()))
	}
}

// probeSandbox — опрос ядра; подменяется в тестах.
var probeSandbox = sandbox.Probe

var warnedIsolation sync.Once

// warnIsolation один раз за процесс говорит, если изоляция неполная.
func warnIsolation(cmd *cobra.Command, report sandbox.Report) {
	gaps := report.Gaps()
	if len(gaps) == 0 {
		return
	}
	warnedIsolation.Do(func() {
		cmd.PrintErrln(i18n.T("внимание: песочница ОС неполная:"))
		for _, gap := range gaps {
			cmd.PrintErrln("  " + gap)
		}
	})
}

// frameReader разбирает кадры ребёнка по мере поступления.
type frameReader struct {
	out, errOut io.Writer
	buf         []byte
	result      *isolatedResult
	err         error
}

func (f *frameReader) Write(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.buf = append(f.buf, p...)
	for len(f.buf) >= 5 {
		kind, size := f.buf[0], binary.BigEndian.Uint32(f.buf[1:5])
		if size > maxFrame {
			f.err = fmt.Errorf("кадр %d байт больше предела", size)
			return 0, f.err
		}
		if uint32(len(f.buf)-5) < size {
			break
		}
		data := f.buf[5 : 5+size]
		switch {
		case f.result != nil:
			f.err = errors.New("данные после итога")
		case kind == frameOut:
			_, _ = f.out.Write(data)
		case kind == frameErr:
			_, _ = f.errOut.Write(data)
		case kind == frameResult:
			var result isolatedResult
			if err := json.Unmarshal(data, &result); err != nil {
				f.err = fmt.Errorf("итог не читается: %w", err)
			} else {
				f.result = &result
			}
		default:
			f.err = fmt.Errorf("неизвестный вид кадра %q", kind)
		}
		if f.err != nil {
			return 0, f.err
		}
		f.buf = f.buf[5+size:]
	}
	return len(p), nil
}

// frameWriter — поток ребёнка, который уходит родителю кадрами.
type frameWriter struct {
	mu   *sync.Mutex
	w    io.Writer
	kind byte
}

func (f frameWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for rest := p; len(rest) > 0; {
		chunk := rest
		if len(chunk) > 1<<20 {
			chunk = chunk[:1<<20]
		}
		if err := writeFrame(f.w, f.kind, chunk); err != nil {
			return len(p) - len(rest), err
		}
		rest = rest[len(chunk):]
	}
	return len(p), nil
}

func writeFrame(w io.Writer, kind byte, data []byte) error {
	header := [5]byte{kind}
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// SandboxChild — работа процесса песочницы: main вызывает её, когда
// sandbox.Enter поставил ограничения. Возвращает код выхода.
func SandboxChild() int {
	var spec isolatedSpec
	if err := json.NewDecoder(io.LimitReader(os.Stdin, maxFrame)).Decode(&spec); err != nil {
		fmt.Fprintf(os.Stderr, "песочница: задание не читается: %v\n", err)
		return 1
	}

	var mu sync.Mutex
	out := frameWriter{mu: &mu, w: os.Stdout, kind: frameOut}
	errOut := frameWriter{mu: &mu, w: os.Stdout, kind: frameErr}
	result := runIsolatedSpec(spec, out, errOut)

	data, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintf(os.Stderr, "песочница: итог не сериализуется: %v\n", err)
		return 1
	}
	mu.Lock()
	defer mu.Unlock()
	if err := writeFrame(os.Stdout, frameResult, data); err != nil {
		fmt.Fprintf(os.Stderr, "песочница: итог не отправлен: %v\n", err)
		return 1
	}
	return 0
}

func runIsolatedSpec(spec isolatedSpec, out, errOut io.Writer) *isolatedResult {
	result := &isolatedResult{}
	fail := func(err error) *isolatedResult {
		result.Error = err.Error()
		return result
	}

	if spec.Mode == "probe" {
		return result
	}
	if spec.Lang != "" {
		_ = i18n.Use(spec.Lang)
	}
	if spec.MaxParallel > 0 {
		viper.Set("defaults.max_parallel_tasks", spec.MaxParallel)
	}
	// Ошибки библиотеки родитель уже показал: второй раз незачем.
	registry, _ := loadAliasRegistry(spec.AliasesDir, spec.ModulesDir)
	box, err := lang.NewSandbox(spec.Roots, spec.Env, spec.Live)
	if err != nil {
		return fail(err)
	}

	interp := lang.NewInterpreter(context.Background(), registry, out)
	interp.SetErr(errOut)
	interp.SetSandbox(box)
	interp.SetProtected(spec.Protected)
	interp.SetDir(spec.Dir)

	switch spec.Mode {
	case "call":
		values := make([]lang.Value, 0, len(spec.Args))
		for _, arg := range spec.Args {
			values = append(values, lang.FromText(arg))
		}
		value, err := interp.CallAlias(spec.Alias, values)
		if err != nil {
			return fail(err)
		}
		if _, null := value.(lang.NullValue); !null {
			result.Value, result.HasValue = lang.Display(value), true
		}
	case "plan", "run":
		file, err := prepareScript(registry, source{Name: spec.Name, Content: spec.Content}, spec.Check)
		if err != nil {
			return fail(err)
		}
		if spec.Mode == "plan" {
			interp.SetPlanMode()
		}
		if err := interp.RunFile(file); err != nil {
			result.Error = err.Error()
		}
	default:
		return fail(fmt.Errorf("неизвестный режим %q", spec.Mode))
	}
	result.Effects = interp.Effects()
	result.Invocations = interp.Invocations()
	return result
}
