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
	"time"

	"octochan/fp"
	"octochan/inventory"
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
	net     []string
	// name — имя сценария, поданного на stdin: оно попадает в сообщения
	// об ошибках, в план и в заявку вместо безликого «stdin».
	name string
	// limit — селекторы --limit: цели сценария исполнителя и хосты,
	// которые видит inventory() сценария на языке алиасов.
	limit []string
	// exec — программы --allow-exec: exec() сценария запускает только их,
	// и только в песочнице ядра.
	exec []string
}

func (o *scriptOptions) register(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.BoolVar(&o.sandbox, "sandbox", false,
		"песочница: файлы только внутри --root, окружение, живые источники и сеть — только разрешённые")
	flags.StringArrayVar(&o.roots, "root", nil,
		"разрешённый каталог песочницы; можно указать несколько раз (по умолчанию — текущий)")
	flags.StringArrayVar(&o.env, "allow-env", nil,
		"разрешить в песочнице переменную окружения; можно указать несколько раз")
	flags.StringArrayVar(&o.live, "allow-live", nil,
		"разрешить в песочнице живые источники этой схемы, например postgres")
	flags.StringArrayVar(&o.net, "allow-net", nil,
		"разрешить в песочнице fetch и curl к этому хосту (host или host:port); можно указать несколько раз")
	flags.StringVar(&o.name, "name", "", "имя сценария, поданного на stdin")
	flags.StringSliceVar(&o.exec, "allow-exec", nil,
		"разрешить exec() сценария запускать эти программы (kubectl,terraform) — только в песочнице ядра; "+
			"оболочки и интерпретаторы не разрешаются")
	flags.StringArrayVar(&o.limit, "limit", nil,
		"только цели (хосты инвентаря), подходящие под селектор: group=db_new, host=db*; можно указать несколько раз")
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
	return o.sandbox || len(o.roots) > 0 || len(o.env) > 0 || len(o.live) > 0 || len(o.net) > 0 || len(o.exec) > 0
}

// build строит песочницу; nil — песочницы нет.
func (o *scriptOptions) build() (*lang.Sandbox, error) {
	if !o.sandbox {
		if o.any() {
			return nil, errors.New("--root, --allow-env, --allow-live, --allow-net и --allow-exec действуют только вместе с --sandbox")
		}
		// Роль с областью выполняет сценарии только в песочнице: без
		// --sandbox она ставится сама, с текущим каталогом.
		if len(scopeOf(currentRole)) == 0 {
			return nil, nil
		}
	}

	roots := o.roots
	if len(roots) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("не удалось определить текущий каталог: %w", err)
		}
		roots = []string{cwd}
	}
	box, err := lang.NewSandbox(roots, o.env, o.live)
	if err != nil {
		return nil, err
	}
	if err := box.AllowNet(o.net); err != nil {
		return nil, err
	}
	if err := checkSandboxRoots(currentRole, box.Roots()); err != nil {
		return nil, err
	}
	var programs []lang.Executable
	for _, name := range o.exec {
		program, err := lang.ResolveExecutable(name, box.Roots())
		if err != nil {
			return nil, err
		}
		programs = append(programs, program)
	}
	box.SetExec(programs)
	return box, nil
}

// scriptSetup — где и как выполняется сценарий.
type scriptSetup struct {
	box *lang.Sandbox
	// dir — каталог для относительных путей; пусто — текущий.
	dir string
	// script — текст сценария: в песочнице его разбирает процесс
	// песочницы заново.
	script source
	// seed и clock — зерно rand() и время now(); нулевые — случайное
	// зерно и текущее время.
	seed  int64
	clock time.Time
	// inventory — хосты, которые увидит inventory() сценария (с --limit —
	// только подходящие); inventoryErr — почему инвентаря нет.
	inventory    []inventory.Host
	inventoryErr error
}

// withInventory читает инвентарь для сценария и сужает его --limit.
// Неправильный --limit — отказ; недоступный инвентарь — ошибка inventory()
// в самом сценарии, если он им пользуется.
func (s scriptSetup) withInventory(cmd *cobra.Command, limit []string) (scriptSetup, error) {
	if _, err := inventory.ParseSelectors(limit); err != nil {
		return s, fmt.Errorf("--limit: %w", err)
	}
	s.inventory, s.inventoryErr = scriptInventory(cmd.Context(), limit)
	if len(limit) > 0 && s.inventoryErr != nil {
		return s, s.inventoryErr
	}
	return s, nil
}

// fixed возвращает установку с зафиксированными зерном и временем: так
// строится план заявки, и с ними же она потом выполняется.
func (s scriptSetup) fixed() scriptSetup {
	if s.seed == 0 {
		s.seed = lang.NewSeed()
	}
	if s.clock.IsZero() {
		s.clock = time.Now()
	}
	return s
}

func (s scriptSetup) interpreter(cmd *cobra.Command, registry *lang.Registry, out io.Writer) *lang.Interpreter {
	interp := lang.NewInterpreter(cmd.Context(), registry, out)
	interp.SetErr(cmd.ErrOrStderr())
	interp.SetSandbox(s.box)
	interp.SetProtected(protectedPaths())
	interp.SetDir(s.dir)
	if s.seed != 0 {
		interp.SetSeed(s.seed)
	}
	if !s.clock.IsZero() {
		interp.SetClock(s.clock)
	}
	interp.SetInventory(s.inventory, s.inventoryErr)
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
	if err := registry.AddFile("", file); err != nil {
		return nil, err
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
	return execScript(cmd, registry, file, setup, true, out)
}

// execScript выполняет сценарий — в режиме плана или по-настоящему.
//
// В песочнице сценарий выполняется отдельным процессом под ограничениями
// ядра: даже обойдя проверки языка, он не дотянется ни до файлов вне
// разрешённых каталогов, ни до сети. План и выполнение идут одним путём,
// поэтому их отпечатки сравнимы.
func execScript(cmd *cobra.Command, registry *lang.Registry, file *lang.File, setup scriptSetup, plan bool, out io.Writer) ([]lang.Effect, []lang.Invocation, error) {
	if setup.box != nil {
		spec := isolatedSpec{Mode: "run", Name: setup.script.Name, Content: setup.script.Content,
			Check: true, Dir: setup.dir, Seed: setup.seed}
		spec.setInventory(setup.inventory, setup.inventoryErr)
		if !setup.clock.IsZero() {
			spec.Clock = setup.clock.Format(time.RFC3339Nano)
		}
		if plan {
			spec.Mode = "plan"
		}
		spec.setBox(setup.box)
		result, err := isolated(cmd, spec, out)
		if err != nil {
			return nil, nil, err
		}
		return result.Effects, result.Invocations, result.err()
	}

	interp := setup.interpreter(cmd, registry, out)
	if plan {
		interp.SetPlanMode()
	}
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
	for _, dep := range lang.Dependencies(file, registry) {
		if dep.Module != "" {
			version := ""
			if pkg, ok := registry.Package(dep.Module); ok {
				version = pkg.Version
			}
			packages[dep.Module] = version
		}
		path := dep.File
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
// проверяются до плана, а утверждения плана и жёсткие правила промпта —
// кольцами и арбитром по плану: заявка на план, который их не прошёл, не
// оформляется.
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
	setup, err := scriptSetup{box: box, dir: dir, script: src}.fixed().withInventory(cmd, opts.limit)
	if err != nil {
		return nil, err
	}

	registry, errs := aliases()
	reportAliasErrors(cmd, errs)

	file, err := loadScript(registry, src)
	if err != nil {
		return nil, err
	}
	if err := checkSkills(prompt, file, registry); err != nil {
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
	judged, err := judgePlan(cmd, purposeRequest, input, prompt, popts.trust, registry, effects, calls)
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
		Prompt:    input.stored(judged),
		Seed:      setup.seed,
		Clock:     setup.clock.Format(time.RFC3339Nano),
		Limit:     opts.limit,
	}
	if box != nil {
		run.Sandboxed = true
		run.Roots = box.Roots()
		run.Env = box.Env()
		run.Live = box.Live()
		run.Net = box.Net()
		for _, p := range box.Exec() {
			run.Exec = append(run.Exec, workspace.ExecProgram{Name: p.Name, Path: p.Path, Sum: p.Sum, Interpreter: p.Interpreter})
		}
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

	setup := scriptSetup{dir: run.Dir, script: source{Name: request.Source, Content: request.Payload}, seed: run.Seed}
	if run.Clock != "" {
		clock, err := time.Parse(time.RFC3339Nano, run.Clock)
		if err != nil {
			return fmt.Errorf("время плана в заявке %s испорчено: %w", request.ID, err)
		}
		setup.clock = clock
	}
	if run.Sandboxed {
		box, err := lang.NewSandbox(run.Roots, run.Env, run.Live)
		if err != nil {
			return fmt.Errorf("песочница заявки недоступна: %w", err)
		}
		if err := box.AllowNet(run.Net); err != nil {
			return fmt.Errorf("песочница заявки недоступна: %w", err)
		}
		// Запускается та же программа, с которой строился план: файл
		// поменялся (обновили kubectl) — выполнение отменяется.
		var programs []lang.Executable
		for _, p := range run.Exec {
			sum, err := lang.FileSum(p.Path)
			if err != nil || sum != p.Sum {
				return fmt.Errorf("программа %s (%s) изменилась после согласования заявки %s: "+
					"запустилась бы не та, что видел план; оформите новую заявку", p.Name, p.Path, request.ID)
			}
			programs = append(programs, lang.Executable{Name: p.Name, Path: p.Path, Sum: p.Sum, Interpreter: p.Interpreter})
		}
		box.SetExec(programs)
		setup.box = box
	}
	// Заявка, оформленная до фиксации зерна и времени, выполняется со
	// случайными: в её плане их не было.
	setup = setup.fixed()
	// Инвентарь читается заново с тем же --limit: поменялся — поменяется
	// план, и сверка с согласованным его поймает.
	setup, err := setup.withInventory(cmd, run.Limit)
	if err != nil {
		return fmt.Errorf("заявка %s: %w", request.ID, err)
	}

	registry, errs := aliases()
	reportAliasErrors(cmd, errs)

	file, err := loadScript(registry, setup.script)
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
		if err := checkSkills(prompt, file, registry); err != nil {
			return fmt.Errorf("заявка %s: %w", request.ID, err)
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

	effects, _, runErr := execScript(cmd, registry, file, setup, false, cmd.OutOrStdout())

	// След остаётся и при ошибке: записанное до сбоя уже записано.
	var (
		written []string
		entry   *workspace.Entry
	)
	for _, effect := range effects {
		if effect.Writes() {
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
		if !effect.Writes() {
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
		case current[i].Kind == lang.EffectExec:
			lines = append(lines, fmt.Sprintf("  %s: запуск с другими аргументами — %s", current[i].Path,
				strings.Join(current[i].Args, " ")))
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

func sortedPaths[V any](set map[string]V) []string {
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
