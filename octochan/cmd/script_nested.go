package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ochan/core/config"
	"ochan/core/i18n"
	"ochan/core/lang"
	"ochan/core/scenario"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Вложенные сценарии: сценарий вызывает сценарий.
//
//	ochan("apply tune.yaml")               плейбук исполнителя
//	apply @ ("tune.yaml" | "login.yaml")   плейбуки с общими строками
//	ochan("run sync.ochan")                сценарий на языке алиасов
//
// Согласованный сценарий не запустит несогласованный: план вложенного
// сценария входит в план внешнего — с отпечатком, в который входит текст
// вложенного файла. Заявку согласуют целиком, со всем, что она запустит;
// поправили вложенный файл после согласования — план другой, и заявка не
// выполнится. Общая строка (string_i: ?) в плане — «из ответа на …»:
// значения при согласовании ещё нет, а откуда оно придёт — видно.

// maxNesting — сколько сценариев могут выполняться один внутри другого.
// Глубже — почти наверняка цикл, которого не видно по именам (алиас,
// который запускает сам себя через run).
const maxNesting = 8

// scriptStack — сценарии, которые выполняются сейчас, от внешнего к
// внутреннему: по нему видно цикл a.ochan → b.ochan → a.ochan.
var scriptStack []string

// enterScript отмечает, что сценарий name начал выполняться (или строиться
// его план), и возвращает, чем отметку снять. Цикл и слишком глубокая
// вложенность — отказ.
func enterScript(name string) (func(), error) {
	key := name
	if name != "" && name != "-" && name != "stdin" && !strings.HasPrefix(name, "<") {
		if abs, err := filepath.Abs(name); err == nil {
			key = abs
		}
		if real, err := lang.RealPath(key); err == nil {
			key = real
		}
		for i, running := range scriptStack {
			if running == key {
				chain := append(append([]string(nil), scriptStack[i:]...), key)
				for j := range chain {
					chain[j] = filepath.Base(chain[j])
				}
				return nil, i18n.Errorf("сценарий запускает сам себя: %s — цикл не кончился бы", strings.Join(chain, " → "))
			}
		}
	}
	if len(scriptStack) >= maxNesting {
		return nil, i18n.Errorf("сценарии вложены глубже %d: %s → %s — похоже на цикл", maxNesting,
			strings.Join(baseNames(scriptStack), " → "), filepath.Base(key))
	}
	scriptStack = append(scriptStack, key)
	return func() { scriptStack = scriptStack[:len(scriptStack)-1] }, nil
}

func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

// nestedFlags — флаги, с которыми команда выполняется как вложенный
// сценарий; значение — берёт ли флаг значение. Остальные — отказ: apply
// --request и --approve выполняют другую заявку, а не сценарий, песочница
// run — отдельный процесс, план которого здесь не построить.
var nestedFlags = map[string]map[string]bool{
	"apply": {"limit": true, "wait": false, "w": false},
	"run":   {"limit": true},
}

// nestedArgs — позиционные аргументы и --limit вложенного apply или run.
func nestedArgs(top string, args []string) (positional, limit []string, err error) {
	allowed := nestedFlags[top]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, value, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		takes, ok := allowed[name]
		if !ok {
			return nil, nil, i18n.Errorf("%s %s из сценария не выполняется: вложенный сценарий — это %s файла с --limit%s",
				top, arg, top, map[bool]string{true: " и --wait", false: ""}[top == "apply"])
		}
		if !takes {
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				return nil, nil, i18n.Errorf("%s %s: нет значения", top, arg)
			}
			i++
			value = args[i]
		}
		if name == "limit" {
			limit = append(limit, value)
		}
	}
	if len(positional) == 0 {
		return nil, nil, i18n.Errorf("%s из сценария: укажите файл сценария", top)
	}
	return positional, limit, nil
}

// nestedKind — вложенный ли это сценарий: apply плейбука или run
// сценария (алиаса). apply --request и прочее — не он.
func nestedKind(argv []string) string {
	target, _, err := rootCmd.Find(argv)
	if err != nil || target == rootCmd {
		return ""
	}
	switch commandKey(target) {
	case "apply", "run":
		return commandKey(target)
	}
	return ""
}

// PlanNested строит план вложенного сценария: его покажет план внешнего,
// а отпечаток войдёт в отпечаток действия. В строке оболочки согласовывать
// нечего, и план не строится.
func (c scriptCommands) PlanNested(ctx context.Context, argv []string, shared *lang.AtShared) (string, string, bool, error) {
	if c.shell && scriptDepth == 0 {
		return "", "", false, nil
	}
	top := nestedKind(argv)
	if top == "" {
		return "", "", false, nil
	}
	positional, limit, err := nestedArgs(top, argv[1:])
	if err != nil {
		return "", "", true, err
	}
	switch top {
	case "apply":
		text, sum, err := planNestedApply(ctx, positional, limit, shared)
		return text, sum, true, err
	default:
		text, sum, err := c.planNestedRun(ctx, positional, limit)
		return text, sum, true, err
	}
}

// planNestedApply — план плейбука исполнителя, как его показал бы ochan
// plan. В отпечаток входит текст плейбука: план исполнителя говорит не
// обо всех параметрах.
func planNestedApply(ctx context.Context, args, limit []string, shared *lang.AtShared) (string, string, error) {
	target, overrides, err := scenarioArgs(args)
	if err != nil {
		return "", "", err
	}
	if target == "-" {
		return "", "", i18n.Errorf("stdin сценарию недоступен — передайте файл")
	}
	src, err := readSource(target)
	if err != nil {
		return "", "", err
	}
	parsed, err := parseScenario(src)
	if err != nil {
		return "", "", err
	}
	parsed = parsed.WithOverrides(overrides)
	if err := resolveTargets(ctx, parsed, limit); err != nil {
		return "", "", err
	}
	runner, err := scenario.Resolve(parsed.Service)
	if err != nil {
		return "", "", err
	}
	plan, err := runner.Plan(ctx, parsed)
	if err != nil {
		return "", "", err
	}
	plan.Source = fmt.Sprintf("%s (%s, исполнитель %s)", src.Name, parsed.Service, runner.Kind())
	if shared != nil {
		params := make([]string, 0, len(shared.Needs))
		for param := range shared.Needs {
			params = append(params, param)
		}
		sort.Strings(params)
		from := i18n.T("другой вызов того же @")
		if len(shared.From) > 0 {
			from = strings.Join(shared.From, i18n.T(" или "))
		}
		for _, param := range params {
			name := param
			if key := shared.Needs[param]; key != param {
				name = i18n.Sprintf("%s (в ответе — %s)", param, key)
			}
			plan.Warn("%s", i18n.Sprintf("общая строка %s ← из ответа на %s: значение появится при выполнении", name, from))
		}
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	material, _ := json.Marshal(struct {
		Content   string
		Overrides []string
		Values    map[string]string
		Limit     []string
		Plan      *config.Plan
	}{src.Content, keys, overrides, limit, plan})
	return plan.Render(60), fmt.Sprintf("%x", sha256.Sum256(material)), nil
}

// planNestedRun — план сценария на языке алиасов (или вызова алиаса) в
// режиме плана, с тем же зерном и временем, что у внешнего: rand() и now()
// дают в плане и при выполнении одно и то же.
func (c scriptCommands) planNestedRun(ctx context.Context, args, limit []string) (string, string, error) {
	registry, _ := aliases()
	runner := &cobra.Command{}
	runner.SetContext(ctx)
	runner.SetErr(io.Discard)
	setup := scriptSetup{seed: c.seed, clock: c.clock}
	setup, err := setup.withInventory(runner, limit)
	if err != nil {
		return "", "", err
	}

	target := args[0]
	if filepath.Ext(target) == lang.AliasExtension && !config.FileExists(target) {
		return "", "", i18n.Errorf("сценария %s нет", target)
	}
	var (
		effects []lang.Effect
		name    string
		extra   string
	)
	if config.FileExists(target) {
		data, err := os.ReadFile(target)
		if err != nil {
			return "", "", err
		}
		src := source{Name: target, Content: string(data)}
		setup.script = src
		file, err := loadScript(registry, src)
		if err != nil {
			return "", "", err
		}
		libraries, _, err := libraryHashes(file, registry)
		if err != nil {
			return "", "", err
		}
		for _, path := range sortedPaths(libraries) {
			extra += path + "=" + libraries[path] + "\x00"
		}
		effects, _, err = planScript(runner, registry, file, setup, io.Discard)
		if err != nil {
			return "", "", i18n.Errorf("%s прервался при построении плана: %v", target, err)
		}
		name = target
	} else {
		// Алиас и его аргументы — как у ochan run алиас ….
		leave, err := enterScript("alias:" + target)
		if err != nil {
			return "", "", err
		}
		defer leave()
		interp := setup.interpreter(runner, registry, io.Discard)
		interp.SetPlanMode()
		values := make([]lang.Value, 0, len(args)-1)
		for _, arg := range args[1:] {
			values = append(values, lang.FromText(arg))
		}
		if _, err := interp.CallAlias(target, values); err != nil {
			return "", "", i18n.Errorf("алиас %s прервался при построении плана: %v", target, err)
		}
		effects = interp.Effects()
		name = "алиас " + strings.Join(args, " ")
	}
	plan := lang.BuildPlan(name, effects)
	material := strings.Join(args, "\x00") + "\x00" + strings.Join(limit, "\x00") + "\x00" + extra +
		strings.Join(fingerprints(effects), "\x00")
	return plan.Render(60), fmt.Sprintf("%x", sha256.Sum256([]byte(material))), nil
}

// Зерно и время внешнего сценария для вложенного run: его выполнение видит
// те же rand() и now(), что его план. Задаёт scriptCommands.Run на время
// команды.
var (
	nestedSeed  int64
	nestedClock time.Time
)

// flagState — значения флагов команды и её подкоманд: их возвращают
// после вложенной команды. Сценарий, который выполняет apply, сам
// выполняется командой apply (или run внутри run): сброс флагов под
// вложенной стёр бы флаги внешней.
type flagState struct {
	flag    *pflag.Flag
	value   string
	slice   []string
	isSlice bool
	changed bool
}

func saveFlags(cmd *cobra.Command) func() {
	var saved []flagState
	save := func(f *pflag.Flag) {
		state := flagState{flag: f, value: f.Value.String(), changed: f.Changed}
		if slice, ok := f.Value.(pflag.SliceValue); ok {
			state.isSlice, state.slice = true, append([]string(nil), slice.GetSlice()...)
		}
		saved = append(saved, state)
	}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		c.Flags().VisitAll(save)
		c.PersistentFlags().VisitAll(save)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cmd)
	return func() {
		for _, s := range saved {
			if s.isSlice {
				_ = s.flag.Value.(pflag.SliceValue).Replace(s.slice)
			} else {
				_ = s.flag.Value.Set(s.value)
			}
			s.flag.Changed = s.changed
		}
	}
}

// sameEffects — те же действия, порядок не важен: f @ (…) выполняет вызов,
// которому не хватало общей строки, когда она нашлась, а план — по порядку
// записи.
func sameEffects(approved, done []string) bool {
	if len(approved) != len(done) {
		return false
	}
	a := append([]string(nil), approved...)
	b := append([]string(nil), done...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkedEffects — действия, которые сверяются с планом: без копий плейбуков с
// общими строками (их в плане нет).
func checkedEffects(effects []lang.Effect) []lang.Effect {
	var out []lang.Effect
	for _, e := range effects {
		if !e.AtCopy {
			out = append(out, e)
		}
	}
	return out
}
