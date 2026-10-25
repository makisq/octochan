package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"octochan/arbiter"
	"octochan/fp"
	"octochan/lang"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

// promptOptions — формальный промпт, по которому написан сценарий, и
// отчёт модели о решениях, которые промпт оставил открытыми.
type promptOptions struct {
	prompt string
	report string
	// bundle — сценарий, промпт и отчёт приходят на stdin одним JSON:
	// у процесса один stdin, а текстов три.
	bundle bool
}

func (o *promptOptions) register(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringVar(&o.prompt, "prompt", "", "формальный промпт: его жёсткие правила проверяются по плану сценария")
	flags.StringVar(&o.report, "report", "", "отчёт модели (раздел report:), проверяется вместе с промптом")
	flags.BoolVar(&o.bundle, "bundle", false,
		"stdin — JSON {\"script\": …, \"prompt\": …, \"report\": …}; так их передаёт ochan-mcp")
}

func (o *promptOptions) any() bool { return o.prompt != "" || o.report != "" || o.bundle }

// scriptInput — сценарий и, если есть, промпт с отчётом.
type scriptInput struct {
	script source
	prompt *source
	report *source
}

// bundle — то, что приходит на stdin с --bundle.
type bundle struct {
	Script string `json:"script"`
	Prompt string `json:"prompt,omitempty"`
	Report string `json:"report,omitempty"`
}

// loadScriptInput читает сценарий, промпт и отчёт.
func loadScriptInput(opts *scriptOptions, popts *promptOptions, target string) (*scriptInput, error) {
	in := &scriptInput{}

	if popts.bundle {
		if target != "-" {
			return nil, errors.New("с --bundle сценарий приходит на stdin: вместо файла укажите «-»")
		}
		if popts.prompt != "" || popts.report != "" {
			return nil, errors.New("с --bundle промпт и отчёт приходят на stdin, а не файлами")
		}
		data, err := readStdin()
		if err != nil {
			return nil, err
		}
		var b bundle
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil {
			return nil, fmt.Errorf("--bundle: на stdin не JSON {script, prompt, report}: %w", err)
		}
		if strings.TrimSpace(b.Script) == "" {
			return nil, errors.New("--bundle: нет сценария")
		}
		name := opts.name
		if name == "" {
			name = "stdin"
		}
		in.script = source{Name: name, Content: b.Script}
		if b.Prompt != "" {
			in.prompt = &source{Name: "prompt.fp", Content: b.Prompt}
		}
		if b.Report != "" {
			in.report = &source{Name: "report.fp", Content: b.Report}
		}
	} else {
		src, err := opts.source(target)
		if err != nil {
			return nil, err
		}
		in.script = src
		stdin := target == "-"
		for _, f := range []struct {
			path string
			dst  **source
		}{{popts.prompt, &in.prompt}, {popts.report, &in.report}} {
			if f.path == "" {
				continue
			}
			if f.path == "-" {
				// Второе чтение stdin отдало бы то же самое: тексты на
				// stdin приходят вместе, через --bundle.
				if stdin {
					return nil, errors.New("stdin может занять только один текст; сценарий, промпт и отчёт вместе — через --bundle")
				}
				stdin = true
			}
			text, err := readSource(f.path)
			if err != nil {
				return nil, err
			}
			*f.dst = &text
		}
	}

	if in.report != nil && in.prompt == nil {
		return nil, errors.New("отчёт проверяется по промпту: укажите и промпт")
	}
	return in, nil
}

// stored — промпт и отчёт для заявки; rules — правила, которые план
// соблюдает. Без промпта — nil.
func (in *scriptInput) stored(rules []string) *workspace.ScriptPrompt {
	if in.prompt == nil {
		return nil
	}
	p := &workspace.ScriptPrompt{Name: in.prompt.Name, Text: in.prompt.Content, Rules: rules}
	if in.report != nil {
		p.ReportName, p.Report = in.report.Name, in.report.Content
	}
	return p
}

// storedInput — промпт и отчёт из заявки.
func storedInput(p *workspace.ScriptPrompt) *scriptInput {
	in := &scriptInput{prompt: &source{Name: p.Name, Content: p.Text}}
	if p.Report != "" {
		in.report = &source{Name: p.ReportName, Content: p.Report}
	}
	return in
}

// promptText — промпт с отчётом в конце: отчёт пишется тем же языком
// и проверяется вместе с промптом. offset — с какой строки начинается
// отчёт, чтобы назвать место ошибки в нём, а не в склейке.
func (in *scriptInput) promptText() (string, int) {
	text := in.prompt.Content
	if in.report == nil {
		return text, 0
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + in.report.Content, strings.Count(text, "\n")
}

// checkPrompt проверяет промпт и отчёт. Без промпта — nil.
func checkPrompt(cmd *cobra.Command, in *scriptInput) (*fp.Result, error) {
	if in.prompt == nil {
		return nil, nil
	}
	opts, err := fpOptions(cmd, false)
	if err != nil {
		return nil, err
	}
	text, offset := in.promptText()
	result := fp.CheckText(in.prompt.Name, text, opts)
	if result.Errors() == 0 {
		return result, nil
	}

	var lines []string
	for _, d := range result.Diagnostics {
		if !d.IsError() {
			continue
		}
		name, pos := in.prompt.Name, d.Pos
		if offset > 0 && pos.Line > offset {
			name, pos.Line = in.report.Name, pos.Line-offset
		}
		lines = append(lines, fmt.Sprintf("  %s:%s: %s: %s", name, pos, d.Code, d.Msg))
		if d.Hint != "" {
			lines = append(lines, "      подсказка: "+d.Hint)
		}
	}
	return nil, fmt.Errorf("промпт не прошёл проверку, отказов: %d (%s)\n%s",
		result.Errors(), countCodes(result.Diagnostics), strings.Join(lines, "\n"))
}

// checkSkills — сценарий по промпту берёт «как» только из скиллов,
// которые промпт подключил: алиас пакета, которого в промпте нет, — то же
// пополнение знания самой моделью, что и операция без скилла (П5).
func checkSkills(prompt *fp.Result, file *lang.File, registry *lang.Registry) error {
	if prompt == nil {
		return nil
	}
	known := map[string]bool{}
	for _, op := range prompt.Knowledge {
		known[op] = true
	}
	missing := map[string][]string{}
	var lines []string
	for _, dep := range lang.Dependencies(file, registry) {
		if dep.Module == "" {
			continue
		}
		if _, ok := prompt.Skills[dep.Module]; !ok {
			missing[dep.Module] = append(missing[dep.Module], dep.Module+"."+dep.Name)
			continue
		}
		// Скилл подключён, но provides: перечисляет не всё: операция вне
		// перечня — тоже вне знания промпта.
		if dep.Kind == "alias" && !known[dep.Name] {
			lines = append(lines, fmt.Sprintf("  сценарий вызывает %s.%s, а в provides: промпта для @%s её нет\n"+
				"      подсказка: добавьте %s в provides: import @%s — или обойдитесь операциями из provides:",
				dep.Module, dep.Name, dep.Module, dep.Name, dep.Module))
		}
	}
	if len(missing) == 0 && len(lines) == 0 {
		return nil
	}
	for _, module := range sortedPaths(missing) {
		version := ""
		if pkg, ok := registry.Package(module); ok && pkg.Version != "" {
			version = " v" + pkg.Version
		}
		lines = append(lines, fmt.Sprintf("  сценарий использует %s, а промпт не подключает @%s\n"+
			"      подсказка: добавьте в промпт import @%s%s — или обойдитесь операциями подключённых скиллов",
			strings.Join(missing[module], ", "), module, module, version))
	}
	return fmt.Errorf("сценарий выходит за знание промпта (П5):\n%s", strings.Join(lines, "\n"))
}

// checkRules проверяет план по жёстким правилам промпта и печатает итог.
// Нарушение — ошибка: план, который нарушает абсолютное правило, не
// оформляется и не выполняется.
func checkRules(cmd *cobra.Command, result *fp.Result, effects []lang.Effect, calls []lang.Invocation) ([]string, error) {
	if result == nil || len(result.Program.Rules) == 0 {
		return nil, nil
	}
	outcomes := arbiter.Check(result.Program.Rules, effects, calls)

	cmd.Println("\nЖёсткие правила промпта:")
	var passed, failed []string
	for _, o := range outcomes {
		rule := arbiter.Describe(o.Rule)
		if o.OK() {
			cmd.Printf("  соблюдено  %s\n", rule)
			passed = append(passed, rule)
			continue
		}
		cmd.Printf("  НАРУШЕНО   %s\n", rule)
		for _, v := range o.Violations {
			cmd.Printf("             %s\n", v)
		}
		if hint := arbiter.Hint(o.Rule); hint != "" {
			cmd.Printf("             подсказка: %s\n", hint)
		}
		failed = append(failed, rule)
	}
	if len(failed) > 0 {
		return nil, fmt.Errorf("план нарушает жёсткие правила промпта: %s", strings.Join(failed, "; "))
	}
	return passed, nil
}

// describeReport — записи отчёта модели одной строкой каждая.
func describeReport(in *scriptInput) []string {
	if in.prompt == nil || in.report == nil {
		return nil
	}
	text, _ := in.promptText()
	prog, _ := fp.Parse(in.prompt.Name, text)
	if prog == nil {
		return nil
	}
	var lines []string
	for _, entry := range prog.Report {
		line := entry.Question
		if entry.Choice != nil {
			line += ": " + entry.Choice.Text
		}
		if len(entry.Alternatives) > 0 {
			line += " из {" + strings.Join(entry.Alternatives, ", ") + "}"
		}
		if entry.Basis != nil {
			line += " — " + entry.Basis.String()
		}
		lines = append(lines, line)
	}
	return lines
}

// printReport печатает решения, которые модель приняла сама.
func printReport(cmd *cobra.Command, in *scriptInput) {
	lines := describeReport(in)
	if len(lines) == 0 {
		return
	}
	cmd.Println("\nОтчёт модели:")
	for _, line := range lines {
		cmd.Printf("  %s\n", line)
	}
}
