package lang

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Запуск программ из сценария — exec(). Только в песочнице ядра и только
// программ, перечисленных в --allow-exec: ядро разрешает запуск ровно этих
// файлов (Landlock), а всё, что программа запустит сама, — нет. Оболочки и
// интерпретаторы не разрешаются вовсе: разрешить sh — значит разрешить всё.
//
// Песочница ограничивает, что можно запустить, а не что запущенная
// программа сделает: она читает и пишет те же каталоги --root и ходит в
// ту же сеть, что и сценарий.

// launchers — оболочки и интерпретаторы: они выполняют произвольный код
// сами, без exec, и ограничение ядра на запуск их не сдерживает. Обёртки
// вроде nice, timeout или env разрешать можно: запустить они могут только
// то, что разрешено и так.
var launchers = []string{
	"sh", "bash", "dash", "zsh", "ksh", "mksh", "csh", "tcsh", "fish", "ash", "busybox", "toybox",
	"perl", "python", "ruby", "node", "nodejs", "deno", "bun", "php", "lua", "luajit", "tclsh", "wish",
	"expect", "guile", "awk", "gawk", "mawk", "nawk", "pwsh", "powershell", "osascript", "ld.so", "ld-linux",
}

// isLauncher — имя программы или её настоящего файла — оболочка,
// интерпретатор или запускалка: python3.11, perl5.36, ld-linux-x86-64.so.2.
func isLauncher(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	for _, l := range launchers {
		if base == l || strings.HasPrefix(base, l+".") || strings.HasPrefix(base, l+"-") ||
			(strings.HasPrefix(base, l) && strings.Trim(base[len(l):], "0123456789.") == "") {
			return true
		}
	}
	return false
}

// Executable — программа, разрешённая --allow-exec.
type Executable struct {
	Name string `json:"name"`
	// Path — настоящий путь файла: запускается он, а не то, что найдётся
	// по PATH внутри песочницы.
	Path string `json:"path"`
	// Sum — SHA-256 файла: заявка выполняется той же программой, с которой
	// строился план.
	Sum string `json:"sum"`
	// Interpreter — загрузчик ELF (ld-linux) динамической программы: ядру
	// нужно разрешение запустить и его. Пусто — программа статическая.
	Interpreter string `json:"interpreter,omitempty"`
}

// ResolveExecutable находит программу по имени (через PATH) или по пути и
// проверяет, что её можно разрешить: обычный файл вне каталогов
// песочницы, не оболочка и не интерпретатор.
func ResolveExecutable(name string, roots []string) (Executable, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Executable{}, errors.New("--allow-exec: пустое имя программы")
	}
	if isLauncher(name) {
		return Executable{}, fmt.Errorf("--allow-exec %s: оболочки, интерпретаторы и запускалки не разрешаются — "+
			"через них выполняется что угодно, и ограничение на запуск теряет смысл", name)
	}
	found, err := exec.LookPath(name)
	if err != nil {
		return Executable{}, fmt.Errorf("--allow-exec %s: программа не найдена: %w", name, err)
	}
	real, err := filepath.EvalSymlinks(found)
	if err != nil {
		return Executable{}, fmt.Errorf("--allow-exec %s: %w", name, err)
	}
	if real, err = filepath.Abs(real); err != nil {
		return Executable{}, err
	}
	if isLauncher(real) {
		return Executable{}, fmt.Errorf("--allow-exec %s: это %s — оболочки и интерпретаторы не разрешаются", name, real)
	}
	info, err := os.Stat(real)
	if err != nil {
		return Executable{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return Executable{}, fmt.Errorf("--allow-exec %s: %s — не исполняемый файл", name, real)
	}
	box := &Sandbox{roots: roots}
	if box.inside(real) {
		return Executable{}, fmt.Errorf("--allow-exec %s: %s лежит в каталоге песочницы — сценарий может его "+
			"переписать и запустить что угодно", name, real)
	}
	sum, err := fileSum(real)
	if err != nil {
		return Executable{}, err
	}
	interp, err := elfInterpreter(real)
	if err != nil {
		return Executable{}, fmt.Errorf("--allow-exec %s: %w", name, err)
	}
	return Executable{Name: filepath.Base(name), Path: real, Sum: sum, Interpreter: interp}, nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FileSum — SHA-256 файла программы: заявка сверяет по нему, та ли это
// программа.
func FileSum(path string) (string, error) { return fileSum(path) }

// elfInterpreter — загрузчик динамической программы ELF; пусто — у
// программы его нет (статическая) или это не ELF (скрипт с #! отвергнет
// ядро: его интерпретатор не разрешён).
func elfInterpreter(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", nil
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		data, err := io.ReadAll(p.Open())
		if err != nil {
			return "", fmt.Errorf("загрузчик ELF не прочитать: %w", err)
		}
		interp := strings.TrimRight(string(data), "\x00")
		real, err := filepath.EvalSymlinks(interp)
		if err != nil {
			return "", fmt.Errorf("загрузчик %s: %w", interp, err)
		}
		return real, nil
	}
	return "", nil
}

// SetExec разрешает exec() программы: ядро уже ограничило запуск ими.
// Без вызова exec() отказывает.
func (s *Sandbox) SetExec(programs []Executable) {
	s.exec = map[string]Executable{}
	for _, p := range programs {
		s.exec[p.Name] = p
	}
}

// Exec — разрешённые программы по имени.
func (s *Sandbox) Exec() []Executable {
	names := make([]string, 0, len(s.exec))
	for name := range s.exec {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Executable, len(names))
	for i, name := range names {
		out[i] = s.exec[name]
	}
	return out
}

// maxExecOutput — сколько вывода программы сценарий получает.
const maxExecOutput = 1 << 20

func execBuiltins() []Builtin {
	return []Builtin{
		{Name: "exec", MinArgs: 1, MaxArgs: -1, Returns: TypeString,
			Doc: "запустить программу из --allow-exec с аргументами и вернуть её вывод; только в песочнице ядра; " +
				"в плане не запускается и возвращает пустую строку",
			Fn: builtinExec},
	}
}

func builtinExec(in *Interpreter, args []Value) (Value, error) {
	name := args[0].Text()
	if in.sandbox == nil || len(in.sandbox.exec) == 0 {
		return nil, fmt.Errorf("exec %s: запуск программ — только в песочнице ядра и только перечисленных: "+
			"--sandbox --allow-exec %s", name, name)
	}
	program, ok := in.sandbox.exec[name]
	if !ok {
		names := make([]string, 0, len(in.sandbox.exec))
		for n := range in.sandbox.exec {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("exec %s: программа не разрешена; разрешены (--allow-exec): %s", name, strings.Join(names, ", "))
	}
	argv := make([]string, len(args)-1)
	for i, a := range args[1:] {
		argv[i] = a.Text()
	}
	in.effects = append(in.effects, Effect{Kind: EffectExec, Path: program.Name, Args: argv, Calls: in.activeCalls()})
	if in.dry != nil {
		return StringValue(""), nil
	}

	cmd := exec.CommandContext(in.ctx, program.Path, argv...)
	cmd.Dir = in.dir
	cmd.Env = in.sandbox.environ()
	// stdin пустой, а не /dev/null: открывать устройства песочница не даёт.
	cmd.Stdin = bytes.NewReader(nil)
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = maxExecOutput, maxExecOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(in.ctx.Err(), context.DeadlineExceeded) || errors.Is(in.ctx.Err(), context.Canceled) {
		return nil, fmt.Errorf("exec %s: прервано: %v", name, in.ctx.Err())
	}
	if err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 2000 {
			tail = "…" + tail[len(tail)-2000:]
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("exec %s: код выхода %d: %s", name, exit.ExitCode(), tail)
		}
		return nil, fmt.Errorf("exec %s: %v", name, err)
	}
	return StringValue(stdout.String()), nil
}

// environ — окружение запускаемой программы: только разрешённые
// переменные, как и для env() сценария.
func (s *Sandbox) environ() []string {
	var out []string
	for _, name := range s.Env() {
		if value, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return out
}

// limitedBuffer — буфер, который перестаёт расти после limit байт.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room < len(p) {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
