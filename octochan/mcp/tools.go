package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// tool — инструмент MCP, за которым стоит ровно одна команда ochan.
type tool struct {
	Name        string
	Title       string
	Description string
	// Command — подкоманда ochan. Список допустимых команд закрыт:
	// см. readCommands и writeCommands.
	Command string
	// Fixed — флаги, которые передаются всегда.
	Fixed []string
	Slots []slot
	// Sandboxed — к команде добавляются флаги песочницы ochan с теми же
	// каталогами, схемами и переменными окружения, что у сервера.
	// Обязательно для инструментов, которые выполняют сценарий, пусть
	// и в режиме плана: чтение файлов в нём настоящее.
	Sandboxed bool
	// Bundle — тексты из слотов идут на stdin одним JSON-объектом
	// {слот: текст}, а в Fixed стоит --bundle: stdin у ochan один, а
	// сценарий, промпт и отчёт — три текста.
	Bundle bool
	// Writes — инструмент что-то создаёт (заявку, документ).
	Writes bool
	// Remote — инструмент ходит во внешнюю систему независимо от
	// аргументов (например, к API исполнителя).
	Remote bool
	// check — проверка готовых позиционных аргументов, которую не выразить
	// одним слотом.
	check func(positional []string) error
}

// readCommands — команды ochan, которые ничего не меняют. У команды
// с подкомандами разрешается подкоманда, а не вся команда: logs rank
// ранжирует журнал внутри разрешённых каталогов, а голый logs показал
// бы журнал самого ochan.
var readCommands = map[string]bool{
	"diff": true, "validate": true, "find": true, "stats": true, "print": true,
	"plan": true, "services": true, "aliases": true, "modules": true,
	"history": true, "requests": true, "status": true, "check": true,
	"fp check": true, "fp protocol": true, "fp words": true, "logs rank": true,
	"incident": true,
}

// commandPath — команда ochan с подкомандой, если первая из постоянных
// частей — не флаг: fp check, logs rank.
func commandPath(t tool) string {
	if len(t.Fixed) > 0 && !strings.HasPrefix(t.Fixed[0], "-") {
		return t.Command + " " + t.Fixed[0]
	}
	return t.Command
}

// writeCommands — единственное, что сервер может создать: заявку в
// состоянии «ждёт решения» — согласует и выполняет её человек — и записи
// инцидента: его открытие, шаги, решение и эскалацию инженерам. Записи
// инцидента ничего в системе не меняют.
var writeCommands = map[string]bool{
	"request": true,
	"incident open": true, "incident note": true, "incident attach": true,
	"incident resolve": true, "incident escalate": true,
}

// checkTable проверяет таблицу инструментов.
//
// Запрет на выполнение держится не на том, что «так написано в таблице»,
// а на этой проверке: команда, которой нет в списках, не пройдёт её даже
// при случайной правке таблицы, и сервер не запустится.
func checkTable(tools []tool) error {
	seen := make(map[string]bool, len(tools))
	for _, t := range tools {
		if seen[t.Name] {
			return fmt.Errorf("инструмент %s объявлен дважды", t.Name)
		}
		seen[t.Name] = true

		switch path := commandPath(t); {
		case readCommands[path] && !t.Writes:
		case writeCommands[path] && t.Writes:
		default:
			return fmt.Errorf("инструмент %s: команда ochan %q не разрешена", t.Name, path)
		}

		if err := checkScriptTool(t); err != nil {
			return err
		}

		if err := checkStdin(t); err != nil {
			return err
		}
	}
	return nil
}

// bundleKeys — тексты, которые ochan принимает с --bundle.
var bundleKeys = map[string]bool{"script": true, "prompt": true, "report": true}

// checkStdin проверяет, как тексты попадают на stdin. stdin у ochan
// один: два текста подряд перемешались бы, поэтому их несколько только
// в JSON-объекте --bundle, и ключи в нём — те, что ochan знает.
func checkStdin(t tool) error {
	texts, script := 0, false
	for _, s := range t.Slots {
		if !stdinSlot(s.Kind) {
			continue
		}
		texts++
		if t.Bundle && !bundleKeys[s.Name] {
			return fmt.Errorf("инструмент %s: текст %s ochan с --bundle не принимает", t.Name, s.Name)
		}
		script = script || s.Kind == slotScript
	}
	flag := false
	for _, f := range t.Fixed {
		flag = flag || f == "--bundle"
	}

	switch {
	case t.Bundle != flag:
		return fmt.Errorf("инструмент %s: Bundle и флаг --bundle задаются вместе", t.Name)
	case t.Bundle && !script:
		return fmt.Errorf("инструмент %s: --bundle несёт сценарий, а слота сценария нет", t.Name)
	case texts > 1 && !t.Bundle:
		return fmt.Errorf("инструмент %s: через stdin передаётся только один текст", t.Name)
	}
	return nil
}

// checkScriptTool проверяет инструменты, принимающие сценарий.
//
// check только читает текст. plan и request выполняют сценарий в режиме
// плана, а чтение файлов, окружения и живых источников в нём настоящее:
// без песочницы модель прочитала бы через план что угодно. Поэтому такой
// инструмент обязан запускать ochan в песочнице и с --script, иначе ochan
// примет текст на stdin за сценарий исполнителя.
func checkScriptTool(t tool) error {
	hasScript := false
	for _, s := range t.Slots {
		if s.Kind == slotScript {
			hasScript = true
		}
	}
	scriptFlag := false
	for _, flag := range t.Fixed {
		if flag == "--script" {
			scriptFlag = true
		}
	}

	switch {
	case !hasScript && !scriptFlag && !t.Sandboxed:
		return nil
	case t.Command == "check" && hasScript && !scriptFlag:
		return nil
	case hasScript && scriptFlag && t.Sandboxed && (t.Command == "plan" || t.Command == "request"):
		return nil
	}
	return fmt.Errorf("инструмент %s: сценарий выполняется только командами plan и request "+
		"с --script и в песочнице", t.Name)
}

// Описания слотов, которые повторяются в нескольких инструментах.
//
// Заголовки и описания инструментов и слотов — по-английски, как и
// протокол: их читает модель. Вывод инструментов — на языке оператора.
func sourceSlot(name, what string, live []string) slot {
	description := what + ": path to a file inside the allowed directories"
	if len(live) > 0 {
		description += " or a live source (" + strings.Join(live, "://, ") + "://)"
	}
	return slot{Name: name, Kind: slotSource, Description: description, Required: true}
}

func scenarioSlot() slot {
	return slot{Name: "scenario", Kind: slotFile, Required: true,
		Description: "scenario file (YAML/JSON with a service field) inside the allowed directories"}
}

func overridesSlot() slot {
	return slot{Name: "overrides", Kind: slotOverrides,
		Description: "scenario parameter overrides: {\"parameter\": \"value\"}"}
}

func scriptSlot() slot {
	return slot{Name: "script", Kind: slotScript, Required: true,
		Description: "script text in the ochan alias language; the aliases tool lists built-in functions and aliases"}
}

func promptSlot() slot {
	return slot{Name: "prompt", Kind: slotText, Required: true,
		Description: "formal prompt text; the language is described in the protocol (protocol tool). " +
			"A report: section is appended to the prompt and checked together with it"}
}

// scriptPromptSlot и reportSlot — промпт, по которому модель написала
// сценарий, и её отчёт. ochan проверяет их до плана, а жёсткие правила
// промпта — по плану, и кладёт в заявку.
func scriptPromptSlot() slot {
	return slot{Name: "prompt", Kind: slotText,
		Description: "the formal prompt the script was written for: it is checked, and its hard rules " +
			"(hard forbid/require) are checked against the plan — a plan that violates them is rejected"}
}

func reportSlot() slot {
	return slot{Name: "report", Kind: slotText,
		Description: "report (report: section) on the decisions the prompt left open; " +
			"checked together with the prompt and stored in the request. Not accepted without prompt"}
}

func nameSlot() slot {
	return slot{Name: "name", Kind: slotName, Flag: "name",
		Description: "script name, for example raise_limit.ochan: it appears in error messages, the plan and the request"}
}

// incidentSlot — инцидент, ради которого заявка: она станет его шагом.
func incidentSlot() slot {
	return slot{Name: "incident", Kind: slotID, Flag: "incident",
		Description: "the incident this request is for: filing and applying it become incident steps; " +
			"refused once the incident is escalated to engineers"}
}

func incidentIDSlot() slot {
	return slot{Name: "id", Kind: slotID, Required: true, Description: "incident ID or its beginning"}
}

// afterSlot — порядок между заявками: новая выполнится только после этой.
func afterSlot() slot {
	return slot{Name: "after", Kind: slotID, Flag: "after",
		Description: "the request runs only after this one is applied; if it fails, the earlier one stays " +
			"applied and the failed one is filed again"}
}

func docSlot() slot {
	return slot{Name: "doc", Kind: slotNewDoc, Flag: "doc",
		Description: "export the request to a new .docx file; an existing file is never overwritten"}
}

// changesNotOverride отсекает второй аргумент, который ochan принял бы за
// переопределение key=value.
//
// ochan отличает «пару конфигов» от «сценария» по второму аргументу: если
// в нём после первого символа есть «=», это переопределение параметра.
// Путь с «=» или ссылка с параметрами (?sslmode=require) молча превратились
// бы в параметр сценария — отказываем явно. Проверяется итоговый аргумент,
// а не то, что прислала модель: относительный путь становится абсолютным,
// и «=» может прийти из имени каталога.
func changesNotOverride(positional []string) error {
	if len(positional) < 2 {
		return nil
	}
	if second := positional[1]; strings.Contains(second[1:], "=") {
		return slotError("changes", "%s contains \"=\", and ochan would take it for a scenario parameter override; "+
			"remove \"=\" from the path or the link parameters", second)
	}
	return nil
}

// buildTools собирает таблицу инструментов.
func buildTools(live []string) []tool {
	return []tool{
		{
			Name:        "diff",
			Title:       "Compare two configurations",
			Description: "Compares configurations by meaning: 8GB and 8192MB, 1s and 1000ms are equal. Writes nothing.",
			Command:     "diff",
			Fixed:       []string{"--fast"},
			Slots: []slot{
				sourceSlot("left", "first configuration", live),
				sourceSlot("right", "second configuration", live),
				{Name: "include_defaults", Kind: slotSwitch, Flag: "all",
					Description: "for live sources, include parameters with default values"},
			},
		},
		{
			Name:        "validate",
			Title:       "Validate a configuration",
			Description: "Checks that a configuration parses and contains parameters.",
			Command:     "validate",
			Slots:       []slot{sourceSlot("source", "configuration", live)},
		},
		{
			Name:        "find",
			Title:       "Find a parameter",
			Description: "Looks for a parameter in every section of a configuration and shows its values.",
			Command:     "find",
			Slots: []slot{
				sourceSlot("source", "configuration", live),
				{Name: "param", Kind: slotParam, Required: true, Description: "parameter name"},
			},
		},
		{
			Name:        "stats",
			Title:       "Configuration statistics",
			Description: "Shows the number of sections and parameters and the notes of the source.",
			Command:     "stats",
			Slots:       []slot{sourceSlot("source", "configuration", live)},
		},
		{
			Name:        "print",
			Title:       "Show a file",
			Description: "Prints the contents of a file as is.",
			Command:     "print",
			Slots: []slot{{Name: "file", Kind: slotFile, Required: true,
				Description: "file inside the allowed directories"}},
		},
		{
			Name:        "plan_config",
			Title:       "Plan a configuration edit",
			Description: "Shows which parameters an edit of base with the values from changes would change. Changes nothing.",
			Command:     "plan",
			Slots: []slot{
				sourceSlot("base", "configuration being edited", live),
				sourceSlot("changes", "configuration with the new values", live),
			},
			check: changesNotOverride,
		},
		{
			Name:  "plan_scenario",
			Title: "Plan a scenario",
			Description: "Shows which operations a scenario would perform and on what. Changing requests " +
				"are forbidden in plan mode at the level of the ochan client.",
			Command: "plan",
			Slots:   []slot{scenarioSlot(), overridesSlot()},
			Remote:  true,
		},
		{
			Name:        "services",
			Title:       "Scenario services",
			Description: "Lists the services that can be put in the service field of a scenario, and their executors.",
			Command:     "services",
		},
		{
			Name:        "aliases",
			Title:       "Aliases and built-in functions",
			Description: "Lists the aliases and built-in functions of the ochan language: the skill library with package versions. A list only, nothing runs.",
			Command:     "aliases",
			Slots: []slot{{Name: "package", Kind: slotPackage,
				Description: "show only this alias package"}},
		},
		{
			Name:        "modules",
			Title:       "Installed modules",
			Description: "Lists the installed ochan command modules.",
			Command:     "modules",
		},
		{
			Name:        "history",
			Title:       "Operation history",
			Description: "Without id, lists operations from newest to oldest; with id, shows the details of an entry.",
			Command:     "history",
			Slots: []slot{
				{Name: "id", Kind: slotID, Description: "entry ID or its beginning"},
				{Name: "limit", Kind: slotLimit, Flag: "limit", Description: "how many entries to show"},
			},
		},
		{
			Name:        "requests",
			Title:       "Requests",
			Description: "Without id, lists requests; with id, shows the request card with its plan.",
			Command:     "requests",
			Slots: []slot{{Name: "id", Kind: slotID,
				Description: "request ID or its beginning"}},
		},
		{
			Name:  "request_config",
			Title: "Request a configuration edit",
			Description: "Creates a request in the \"pending\" state to edit base with the values from changes. " +
				"The request itself changes nothing: a human approves and applies it.",
			Command: "request",
			Slots: []slot{
				sourceSlot("base", "configuration being edited", live),
				sourceSlot("changes", "configuration with the new values", live),
				afterSlot(),
				incidentSlot(),
				docSlot(),
			},
			Writes: true,
			check:  changesNotOverride,
		},
		{
			Name:  "request_scenario",
			Title: "Request a scenario run",
			Description: "Creates a request in the \"pending\" state to run a scenario. The request stores " +
				"the scenario text. The request itself runs nothing: a human approves and applies it.",
			Command: "request",
			Slots:   []slot{scenarioSlot(), overridesSlot(), afterSlot(), incidentSlot(), docSlot()},
			Writes:  true,
			Remote:  true,
		},
		{
			Name:  "protocol",
			Title: "Protocol",
			Description: "The protocol of working with ochan: what the model is here, how the operator states " +
				"a task (a formal prompt, or natural language when the operator set fp.mode: natural), refusals " +
				"and the work cycle. The server also gives it in instructions on connection; the tool is for " +
				"clients that do not show instructions to the model.",
			Command: "fp",
			Fixed:   []string{"protocol"},
		},
		{
			Name:  "vocabulary",
			Title: "Vocabulary of the formal prompt",
			Description: "The closed vocabulary of the formal prompt language: every meaningful word, its senses " +
				"(classes) and where it comes from. Without arguments, the base vocabulary and the operator's; with " +
				"skill, the words that skill brings into a prompt that imports it; with word, the entries of that word " +
				"in every vocabulary. A word outside the vocabulary is ⊥syn.",
			Command: "fp",
			Fixed:   []string{"words"},
			Slots: []slot{
				{Name: "skill", Kind: slotPackage, Flag: "skill", Description: "show the vocabulary of this skill"},
				{Name: "word", Kind: slotParam, Description: "show the entries of this word"},
			},
		},
		{
			Name:  "check_prompt",
			Title: "Check a formal prompt",
			Description: "A deterministic check before the model: the text belongs to the language (⊥syn), words " +
				"are unambiguous (⊥ambig), knowledge is sufficient — Req(P) ⊆ K (⊥undef), hard rules are " +
				"consistent (⊥conflict). Skills are matched against the alias package library. With a report: " +
				"section, the report is checked too.",
			Command: "fp",
			Fixed:   []string{"check"},
			Slots:   []slot{promptSlot(), nameSlot()},
		},
		{
			Name:  "check_script",
			Title: "Check a script",
			Description: "Checks an alias-language script without running anything: syntax, unknown names and " +
				"functions, the number of arguments, break and return out of place, clearly wrong types, a " +
				"variable assigned a value of another type. Finds errors in branches that would not run, too. " +
				"Reports all errors at once.",
			Command: "check",
			Slots:   []slot{scriptSlot(), nameSlot()},
		},
		{
			Name:  "plan_script",
			Title: "Plan an alias-language script",
			Description: "Checks a script and runs it in plan mode: write, save and print to a file write nothing, " +
				"and the plan shows which files and parameters would change. The script runs in the server " +
				"sandbox: files only inside the allowed directories, environment variables, live sources and " +
				"network hosts only as allowed. Prints what the script printed and the plan. With prompt, checks the hard " +
				"rules of the prompt against the plan.",
			Command:   "plan",
			Fixed:     []string{"--script", "--bundle"},
			Sandboxed: true,
			Bundle:    true,
			Slots:     []slot{scriptSlot(), scriptPromptSlot(), reportSlot(), nameSlot()},
		},
		{
			Name:  "request_script",
			Title: "Request an alias-language script run",
			Description: "Checks a script, builds the plan in the server sandbox and creates a request in the " +
				"\"pending\" state. The request stores the script text, the plan and the sandbox. The request " +
				"itself runs nothing: a human approves and applies it, and before applying the plan is built " +
				"again and must match the approved one. With prompt and report, the request stores the prompt " +
				"and the report, and no request is created for a plan that violates a hard rule of the prompt.",
			Command:   "request",
			Fixed:     []string{"--script", "--bundle"},
			Sandboxed: true,
			Bundle:    true,
			Slots:     []slot{scriptSlot(), scriptPromptSlot(), reportSlot(), nameSlot(), afterSlot(), incidentSlot(), docSlot()},
			Writes:    true,
		},
		{
			Name:  "incidents",
			Title: "Incidents",
			Description: "Without id, lists incidents with the time left to fix each; with id, shows the incident " +
				"card: level, deadline, steps, attached logs, outcome.",
			Command: "incident",
			Slots:   []slot{{Name: "id", Kind: slotID, Description: "incident ID or its beginning"}},
		},
		{
			Name:  "incident_open",
			Title: "Open an incident",
			Description: "Opens an incident you detected. Its level sets how long you may fix it yourself. When the " +
				"time is up, or after incident_escalate, it goes to the engineering group with your steps and logs, " +
				"and you stop acting on it: do not try to fix it further.",
			Command: "incident",
			Fixed:   []string{"open"},
			Slots: []slot{
				{Name: "level", Kind: slotParam, Flag: "level", Required: true,
					Description: "criticality level from incidents.levels, for example low, medium, high, critical"},
				{Name: "host", Kind: slotParam, Flag: "host", Description: "affected host"},
				{Name: "summary", Kind: slotText, Required: true, Description: "what happened, for engineers to read"},
			},
			Writes: true,
		},
		{
			Name:  "incident_note",
			Title: "Note an incident step",
			Description: "Records what you found or did. Engineers read these notes if the incident is escalated: " +
				"write facts, hypotheses you checked and their results.",
			Command: "incident",
			Fixed:   []string{"note"},
			Slots:   []slot{incidentIDSlot(), {Name: "text", Kind: slotText, Required: true, Description: "the step"}},
			Writes:  true,
		},
		{
			Name:        "incident_attach",
			Title:       "Attach a log to an incident",
			Description: "Attaches a log file; it goes to the engineers with the escalation. Up to 256 KiB per file.",
			Command:     "incident",
			Fixed:       []string{"attach"},
			Slots: []slot{incidentIDSlot(),
				{Name: "log", Kind: slotFile, Required: true, Description: "log file inside the allowed directories"}},
			Writes: true,
		},
		{
			Name:        "incident_resolve",
			Title:       "Resolve an incident",
			Description: "Closes the incident as fixed. Refused once it is escalated: then engineers own it.",
			Command:     "incident",
			Fixed:       []string{"resolve"},
			Slots: []slot{incidentIDSlot(),
				{Name: "summary", Kind: slotText, Required: true, Description: "what fixed it and how you checked"}},
			Writes: true,
		},
		{
			Name:  "incident_escalate",
			Title: "Escalate an incident to engineers",
			Description: "Hands the incident to the engineering group now, when your knowledge is not enough: do it " +
				"instead of repeating attempts. The engineers get the description, your steps and the logs.",
			Command: "incident",
			Fixed:   []string{"escalate"},
			Slots: []slot{incidentIDSlot(),
				{Name: "reason", Kind: slotText, Required: true, Description: "why you cannot fix it"}},
			Writes: true,
		},
		{
			Name:  "rank_logs",
			Title: "Rank a log by severity",
			Description: "Determines the level of every log line by the rules from config.yaml (logs.rules — " +
				"regular expressions in order, the first match decides) and shows lines of level min and more " +
				"important. With summary — a summary: lines per level and the most frequent messages. The log " +
				"is a file inside the allowed directories.",
			Command: "logs",
			Fixed:   []string{"rank"},
			Slots: []slot{
				{Name: "file", Kind: slotFile, Required: true, Description: "log file inside the allowed directories"},
				{Name: "min", Kind: slotParam, Flag: "min",
					Description: "level below which lines are not shown, for example warning or error " +
						"(levels are logs.levels; logs.min_level by default)"},
				{Name: "summary", Kind: slotSwitch, Flag: "summary",
					Description: "a summary instead of lines: lines per level and frequent messages"},
				{Name: "top", Kind: slotLimit, Flag: "top", Description: "how many frequent messages to show in the summary"},
			},
		},
		{
			Name:        "task_status",
			Title:       "Task status",
			Description: "Shows the status and log of a task in the executor's external system.",
			Command:     "status",
			Slots: []slot{
				{Name: "task_id", Kind: slotTaskID, Required: true, Description: "task ID"},
				{Name: "executor", Kind: slotParam, Flag: "executor",
					Description: "the executor that created the task, for example rlm; needed only when several executors can show a status"},
			},
			Remote: true,
		},
	}
}

// argv проверяет аргументы вызова и строит командную строку ochan и то,
// что пойдёт ему на stdin.
//
// Порядок фиксирован: подкоманда, постоянные флаги, флаги песочницы, флаги
// из слотов, затем «--» и позиционные аргументы. После «--» ochan не читает
// флаги, поэтому значение слота не может стать флагом, даже если
// начинается с дефиса.
func (t *tool) argv(box *sandbox, args map[string]any) ([]string, string, error) {
	known := make(map[string]bool, len(t.Slots))
	for _, s := range t.Slots {
		known[s.Name] = true
	}

	var unknown []string
	for name := range args {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, "", fmt.Errorf("unknown parameters: %s", strings.Join(unknown, ", "))
	}

	command := append([]string{t.Command}, t.Fixed...)
	if t.Sandboxed {
		command = append(command, box.ochanFlags()...)
	}
	var (
		positional []string
		stdin      string
		texts      = map[string]string{}
	)

	for _, s := range t.Slots {
		raw, present := args[s.Name]
		if !present || raw == nil {
			if s.Required {
				return nil, "", slotError(s.Name, "a required parameter is missing")
			}
			continue
		}

		if stdinSlot(s.Kind) {
			text, err := scriptText(s.Name, raw)
			if err != nil {
				return nil, "", err
			}
			if t.Bundle {
				texts[s.Name] = text
				continue
			}
			stdin = text
			positional = append(positional, "-")
			continue
		}

		values, err := box.fill(s, raw)
		if err != nil {
			return nil, "", err
		}
		if s.Flag != "" {
			command = append(command, values...)
		} else {
			positional = append(positional, values...)
		}
	}

	if t.Bundle {
		// Ключи json.Marshal сортирует: одинаковый вызов — одинаковый stdin.
		data, err := json.Marshal(texts)
		if err != nil {
			return nil, "", err
		}
		stdin = string(data)
		positional = append(positional, "-")
	}

	if t.check != nil {
		if err := t.check(positional); err != nil {
			return nil, "", err
		}
	}

	if len(positional) > 0 {
		command = append(command, "--")
		command = append(command, positional...)
	}
	return command, stdin, nil
}

// schema описывает входные параметры инструмента в JSON Schema.
func (t *tool) schema() map[string]any {
	properties := make(map[string]any, len(t.Slots))
	required := []string{}

	for _, s := range t.Slots {
		property := map[string]any{"description": s.Description}
		switch s.Kind {
		case slotSwitch:
			property["type"] = "boolean"
		case slotLimit:
			property["type"] = "integer"
			property["minimum"] = 1
			property["maximum"] = maxLimit
		case slotOverrides:
			property["type"] = "object"
			property["additionalProperties"] = map[string]any{"type": "string"}
		case slotParam:
			property["type"] = "string"
			property["pattern"] = paramPattern.String()
		case slotID:
			property["type"] = "string"
			property["pattern"] = idPattern.String()
		case slotTaskID:
			property["type"] = "string"
			property["pattern"] = taskIDPattern.String()
		case slotName:
			property["type"] = "string"
			property["pattern"] = namePattern.String()
		case slotScript, slotText:
			property["type"] = "string"
			property["minLength"] = 1
			property["maxLength"] = maxScript
		default:
			property["type"] = "string"
		}
		properties[s.Name] = property
		if s.Required {
			required = append(required, s.Name)
		}
	}

	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

// openWorld сообщает, может ли инструмент обратиться во внешнюю систему.
func (t *tool) openWorld(liveEnabled bool) bool {
	if t.Remote {
		return true
	}
	if !liveEnabled {
		return false
	}
	// Сценарий, который выполняется хотя бы в режиме плана, может читать
	// разрешённые живые источники.
	if t.Sandboxed {
		return true
	}
	for _, s := range t.Slots {
		if s.Kind == slotSource {
			return true
		}
	}
	return false
}

// descriptor — описание инструмента для tools/list.
func (t *tool) descriptor(liveEnabled bool) map[string]any {
	annotations := map[string]any{
		"title":         t.Title,
		"readOnlyHint":  !t.Writes,
		"openWorldHint": t.openWorld(liveEnabled),
	}
	if t.Writes {
		// Заявка — новая запись, а не перезапись: ничего не разрушает,
		// но повторный вызов создаёт ещё одну заявку.
		annotations["destructiveHint"] = false
		annotations["idempotentHint"] = false
	}

	return map[string]any{
		"name":        t.Name,
		"title":       t.Title,
		"description": t.Description,
		"inputSchema": t.schema(),
		"annotations": annotations,
	}
}
