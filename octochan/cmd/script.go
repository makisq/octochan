package cmd

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"octochan/fp"
	"octochan/lang"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

// scriptOptions — песочница для сценария на языке алиасов.
//
// Без флагов сценарий умеет всё, что умеет запустивший его человек. С
// --sandbox встроенные функции видят только разрешённые каталоги,
// переменные окружения и живые источники — так запускаются сценарии,
// которые написал не человек.
type scriptOptions struct {
	sandbox bool
	roots   []string
	env     []string
	live    []string
	// name — имя сценария, поданного на stdin: оно попадает в сообщения
	// об ошибках, в план и в заявку вместо безликого «stdin».
	name string
}

func (o *scriptOptions) register(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.BoolVar(&o.sandbox, "sandbox", false,
		"песочница: файлы только внутри --root, окружение и живые источники — только разрешённые")
	flags.StringArrayVar(&o.roots, "root", nil,
		"разрешённый каталог песочницы; можно указать несколько раз (по умолчанию — текущий)")
	flags.StringArrayVar(&o.env, "allow-env", nil,
		"разрешить в песочнице переменную окружения; можно указать несколько раз")
	flags.StringArrayVar(&o.live, "allow-live", nil,
		"разрешить в песочнице живые источники этой схемы, например postgres")
	flags.StringVar(&o.name, "name", "", "имя сценария, поданного на stdin")
}

// source читает сценарий из файла или stdin.
func (o *scriptOptions) source(target string) (source, error) {
	if o.name != "" && target != "-" {
		return source{}, errors.New("--name задаёт имя сценария, поданного на stdin; у файла имя уже есть")
	}
	src, err := readSource(target)
	if err != nil {
		return source{}, err
	}
	if o.name != "" {
		src.Name = o.name
	}
	return src, nil
}

func (o *scriptOptions) any() bool {
	return o.sandbox || len(o.roots) > 0 || len(o.env) > 0 || len(o.live) > 0
}

// build строит песочницу; nil — песочницы нет.
func (o *scriptOptions) build() (*lang.Sandbox, error) {
	if !o.sandbox {
		if o.any() {
			return nil, errors.New("--root, --allow-env и --allow-live действуют только вместе с --sandbox")
		}
		return nil, nil
	}

	roots := o.roots
	if len(roots) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("не удалось определить текущий каталог: %w", err)
		}
		roots = []string{cwd}
	}
	return lang.NewSandbox(roots, o.env, o.live)
}

// scriptSetup — где и как выполняется сценарий.
type scriptSetup struct {
	box *lang.Sandbox
	// dir — каталог для относительных путей; пусто — текущий.
	dir string
}

func (s scriptSetup) interpreter(cmd *cobra.Command, registry *lang.Registry, out io.Writer) *lang.Interpreter {
	interp := lang.NewInterpreter(cmd.Context(), registry, out)
	interp.SetErr(cmd.ErrOrStderr())
	interp.SetSandbox(s.box)
	interp.SetDir(s.dir)
	return interp
}

// loadScript разбирает сценарий, регистрирует его алиасы и проверяет его
// до выполнения.
//
// Проверка обязательна везде, где сценарий планируют или согласуют: план
// сценария, который упадёт на опечатке в непройденной ветке, обещает то,
// чего не будет.
func loadScript(registry *lang.Registry, src source) (*lang.File, error) {
	file, err := lang.Parse(src.Name, src.Content)
	if err != nil {
		return nil, err
	}
	for _, decl := range file.Aliases {
		if err := registry.Add("", decl); err != nil {
			return nil, err
		}
	}

	if problems := lang.Check(file, registry); len(problems) > 0 {
		lines := make([]string, len(problems))
		for i, problem := range problems {
			lines[i] = "  " + problem.String()
		}
		return nil, fmt.Errorf("сценарий не прошёл проверку, ошибок: %d\n%s",
			len(problems), strings.Join(lines, "\n"))
	}
	return file, nil
}

// planScript выполняет сценарий в режиме плана и возвращает его действия
// и вызовы алиасов — по ним проверяются жёсткие правила промпта.
//
// План сценария, прервавшегося ошибкой, неполон: это говорится в самом
// плане, а не замалчивается.
func planScript(cmd *cobra.Command, registry *lang.Registry, file *lang.File, setup scriptSetup, out io.Writer) ([]lang.Effect, []lang.Invocation, error) {
	interp := setup.interpreter(cmd, registry, out)
	interp.SetPlanMode()
	err := interp.RunFile(file)
	return interp.Effects(), interp.Invocations(), err
}

// fingerprints — отпечатки действий, по которым план сверяется с выполнением.
func fingerprints(effects []lang.Effect) []string {
	prints := make([]string, len(effects))
	for i, effect := range effects {
		prints[i] = effect.Fingerprint()
	}
	return prints
}

// libraryHashes — суммы файлов библиотеки, от которых зависит сценарий,
// и версии их пакетов.
func libraryHashes(file *lang.File, registry *lang.Registry) (map[string]string, map[string]string, error) {
	hashes := make(map[string]string)
	packages := make(map[string]string)
	for _, decl := range lang.Uses(file, registry) {
		if decl.Module != "" {
			version := ""
			if pkg, ok := registry.Package(decl.Module); ok {
				version = pkg.Version
			}
			packages[decl.Module] = version
		}
		path := decl.Pos.File
		if _, done := hashes[path]; done {
			continue
		}
		sum, err := fileHash(path)
		if err != nil {
			return nil, nil, err
		}
		hashes[path] = sum
	}
	if len(hashes) == 0 {
		hashes = nil
	}
	if len(packages) == 0 {
		packages = nil
	}
	return hashes, packages, nil
}

func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать %s: %w", path, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// scriptRequest оформляет заявку на выполнение сценария на языке алиасов.
//
// Если сценарий написан по формальному промпту, промпт и отчёт модели
// проверяются до плана, а жёсткие правила промпта — по плану: заявка на
// план, который нарушает абсолютное правило, не оформляется.
func scriptRequest(cmd *cobra.Command, args []string, opts *scriptOptions, popts *promptOptions) (*workspace.Request, error) {
	if len(args) > 1 {
		return nil, errors.New("сценарий на языке алиасов параметров не принимает: значения пишутся в его тексте")
	}
	target, err := argOrStdin(args, 0)
	if err != nil {
		return nil, fmt.Errorf("укажите файл сценария или подайте его на stdin: %w", err)
	}
	input, err := loadScriptInput(opts, popts, target)
	if err != nil {
		return nil, err
	}
	src := input.script
	prompt, err := checkPrompt(cmd, input)
	if err != nil {
		return nil, err
	}

	box, err := opts.build()
	if err != nil {
		return nil, err
	}
	dir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("не удалось определить текущий каталог: %w", err)
	}
	setup := scriptSetup{box: box, dir: dir}

	registry, errs := aliases()
	reportAliasErrors(cmd, errs)

	file, err := loadScript(registry, src)
	if err != nil {
		return nil, err
	}

	var output strings.Builder
	effects, calls, err := planScript(cmd, registry, file, setup, &output)
	if output.Len() > 0 {
		cmd.Print("Вывод сценария в режиме плана:\n" + output.String() + "\n")
	}
	if err != nil {
		// Заявка на сценарий, который падает уже при планировании, — это
		// заявка на ошибку: согласовывать нечего.
		return nil, fmt.Errorf("сценарий прервался при построении плана: %w", err)
	}
	rules, err := checkRules(cmd, prompt, effects, calls)
	if err != nil {
		return nil, err
	}

	libraries, packages, err := libraryHashes(file, registry)
	if err != nil {
		return nil, err
	}

	plan := lang.BuildPlan(src.Name, effects)
	run := &workspace.ScriptRun{
		Dir:       dir,
		Libraries: libraries,
		Packages:  packages,
		Effects:   fingerprints(effects),
		Prompt:    input.stored(rules),
	}
	if box != nil {
		run.Sandboxed = true
		run.Roots = box.Roots()
		run.Env = box.Env()
		run.Live = box.Live()
	}

	return &workspace.Request{
		Source:  src.Name,
		Summary: fmt.Sprintf("сценарий %s, изменений: %d", filepath.Base(src.Name), plan.Len()),
		Plan:    plan,
		Payload: src.Content,
		Script:  run,
	}, nil
}

// applyScriptRequest выполняет согласованную заявку на сценарий.
//
// Выполняется ровно то, на что сказали «да»: текст сценария из заявки,
// в той же песочнице и с тем же каталогом. Если с момента согласования
// поменялся алиас библиотеки, который сценарий вызывает, или план стал
// другим (поправили файл, который сценарий читает), выполнение отменяется:
// согласовывали не это.
func applyScriptRequest(cmd *cobra.Command, store *workspace.Requests, request *workspace.Request) error {
	run := request.Script

	for _, path := range sortedPaths(run.Libraries) {
		sum, err := fileHash(path)
		if err != nil || sum != run.Libraries[path] {
			return fmt.Errorf("файл алиасов %s изменился после согласования заявки %s: "+
				"выполнилось бы не то, что согласовали; оформите новую заявку", path, request.ID)
		}
	}

	var setup scriptSetup
	setup.dir = run.Dir
	if run.Sandboxed {
		box, err := lang.NewSandbox(run.Roots, run.Env, run.Live)
		if err != nil {
			return fmt.Errorf("песочница заявки недоступна: %w", err)
		}
		setup.box = box
	}

	registry, errs := aliases()
	reportAliasErrors(cmd, errs)

	file, err := loadScript(registry, source{Name: request.Source, Content: request.Payload})
	if err != nil {
		return fmt.Errorf("сохранённый в заявке сценарий: %w", err)
	}

	// Промпт проверяется заново: скилл, на который он ссылается, мог
	// пропасть из библиотеки или смениться версией.
	var prompt *fp.Result
	if run.Prompt != nil {
		prompt, err = checkPrompt(cmd, storedInput(run.Prompt))
		if err != nil {
			return fmt.Errorf("промпт заявки %s: %w", request.ID, err)
		}
	}

	planned, calls, err := planScript(cmd, registry, file, setup, io.Discard)
	if err != nil {
		return fmt.Errorf("сценарий прервался при повторном построении плана: %w", err)
	}
	if !slices.Equal(fingerprints(planned), run.Effects) {
		current := lang.BuildPlan(request.Source, planned)
		return fmt.Errorf("план сценария изменился после согласования заявки %s: "+
			"выполнилось бы не то, что согласовали; оформите новую заявку.\n%s\nТекущий план:\n%s",
			request.ID, describeDrift(run.Effects, planned), current.Render(60))
	}
	if _, err := checkRules(cmd, prompt, planned, calls); err != nil {
		return fmt.Errorf("заявка %s: %w", request.ID, err)
	}

	cmd.Printf("Заявка %s, сценарий %s\n", request.ID, request.Source)

	interp := setup.interpreter(cmd, registry, cmd.OutOrStdout())
	runErr := interp.RunFile(file)
	effects := interp.Effects()

	// След остаётся и при ошибке: записанное до сбоя уже записано.
	var (
		written []string
		entry   *workspace.Entry
	)
	for _, effect := range effects {
		if effect.Kind != lang.EffectLive {
			written = append(written, effect.Path)
		}
	}
	if runErr == nil || len(written) > 0 {
		if err := store.MarkApplied(request, written); err != nil {
			cmd.PrintErrf("внимание: не удалось отметить заявку выполненной: %v\n", err)
		}
		// Файлы до сценария — в журнал: по ним выполнение откатывается,
		// в том числе прерванное.
		files := fileStates(effects)
		if size := stateBytes(files); size > maxRollbackBytes {
			cmd.PrintErrf("внимание: откат недоступен: прежнее содержимое файлов занимает %d МиБ, "+
				"журнал хранит не больше %d МиБ\n", size>>20, maxRollbackBytes>>20)
			files = nil
		}
		done := lang.BuildPlan(request.Source, effects)
		entry = &workspace.Entry{
			Kind:       workspace.EntryScript,
			Command:    "apply --request " + request.ID,
			Source:     request.Source,
			Summary:    fmt.Sprintf("сценарий %s, изменений: %d", filepath.Base(request.Source), done.Len()),
			Changes:    done.Changes,
			Files:      files,
			Reversible: len(files) > 0,
		}
		record(cmd, entry)
		notify(cmd, fmt.Sprintf("Заявка %s выполнена", request.ID), requestMailBody(request), nil)
	}
	if runErr != nil {
		if entry != nil && entry.Reversible && entry.ID != "" {
			cmd.PrintErrf("сценарий прерван; записанное до сбоя откатывается: ochan rollback %s\n", entry.ID)
		}
		return runErr
	}

	if !slices.Equal(fingerprints(effects), run.Effects) {
		cmd.PrintErrln("внимание: выполнение разошлось с согласованным планом: файлы поменялись во время выполнения")
	}
	cmd.Printf("Заявка %s выполнена, записано файлов: %d\n", request.ID, len(written))
	if entry != nil && entry.Reversible && entry.ID != "" {
		cmd.Printf("Откат: ochan rollback %s\n", entry.ID)
	}
	return nil
}

// maxRollbackBytes — сколько прежнего содержимого файлов журнал хранит
// для отката одного сценария. Конфиги весят килобайты; предел отсекает
// сценарий, который переписал гигабайтный файл, а не настоящие правки.
const maxRollbackBytes = 16 << 20

// fileStates — файлы, которые записал сценарий, в состоянии до него.
// Файл, записанный дважды, помнит состояние до первой записи и то, что
// оставила последняя.
func fileStates(effects []lang.Effect) []workspace.FileState {
	index := make(map[string]int)
	var states []workspace.FileState
	for _, effect := range effects {
		if effect.Kind == lang.EffectLive {
			continue
		}
		i, seen := index[effect.Path]
		if !seen {
			i = len(states)
			index[effect.Path] = i
			states = append(states, workspace.FileState{
				Path: effect.Path, Existed: effect.Existed, Content: effect.Before})
		}
		states[i].After = workspace.StateHash(true, effect.After)
	}
	return states
}

func stateBytes(states []workspace.FileState) int {
	size := 0
	for _, state := range states {
		size += len(state.Content)
	}
	return size
}

// describeDrift объясняет, чем текущий план отличается от согласованного.
//
// Сам план этого не покажет: если сценарий строит конфиг из файла, который
// поправили, изменения в плане могут выглядеть так же, а записано будет
// другое содержимое.
func describeDrift(approved []string, current []lang.Effect) string {
	var lines []string
	if len(approved) != len(current) {
		lines = append(lines, fmt.Sprintf("  действий было %d, стало %d", len(approved), len(current)))
	}
	for i := 0; i < len(approved) && i < len(current); i++ {
		now := current[i].Fingerprint()
		if approved[i] == now {
			continue
		}
		wasKind, wasPath := splitFingerprint(approved[i])
		switch {
		case wasKind != string(current[i].Kind) || wasPath != current[i].Path:
			lines = append(lines, fmt.Sprintf("  действие %d: было %s %s, стало %s %s",
				i+1, wasKind, wasPath, current[i].Kind, current[i].Path))
		default:
			lines = append(lines, fmt.Sprintf("  %s: будет записано не то содержимое, что согласовали "+
				"(поменялись данные, из которых сценарий его строит)", current[i].Path))
		}
	}
	return "Что изменилось:\n" + strings.Join(lines, "\n")
}

// splitFingerprint разбирает отпечаток «вид путь сумма»; путь может
// содержать пробелы, поэтому вид и сумма отделяются по краям.
func splitFingerprint(print string) (kind, path string) {
	kind, rest, _ := strings.Cut(print, " ")
	if kind == string(lang.EffectLive) {
		return kind, rest
	}
	if i := strings.LastIndex(rest, " "); i >= 0 {
		rest = rest[:i]
	}
	return kind, rest
}

func sortedPaths(set map[string]string) []string {
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
