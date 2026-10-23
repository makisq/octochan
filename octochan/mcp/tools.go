package main

import (
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
	// Writes — инструмент что-то создаёт (заявку, документ).
	Writes bool
	// Remote — инструмент ходит во внешнюю систему независимо от
	// аргументов (например, к API исполнителя).
	Remote bool
	// check — проверка готовых позиционных аргументов, которую не выразить
	// одним слотом.
	check func(positional []string) error
}

// readCommands — команды ochan, которые ничего не меняют.
var readCommands = map[string]bool{
	"diff": true, "validate": true, "find": true, "stats": true, "print": true,
	"plan": true, "services": true, "aliases": true, "modules": true,
	"history": true, "requests": true, "status": true, "check": true, "fp": true,
}

// writeCommands — единственное, что сервер может создать: заявку в
// состоянии «ждёт решения». Согласует и выполняет её человек.
var writeCommands = map[string]bool{
	"request": true,
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

		switch {
		case readCommands[t.Command] && !t.Writes:
		case writeCommands[t.Command] && t.Writes:
		default:
			return fmt.Errorf("инструмент %s: команда ochan %q не разрешена", t.Name, t.Command)
		}

		if err := checkScriptTool(t); err != nil {
			return err
		}

		// stdin у ochan один: два текста в одном вызове перемешались бы.
		stdin := 0
		for _, s := range t.Slots {
			if stdinSlot(s.Kind) {
				stdin++
			}
		}
		if stdin > 1 {
			return fmt.Errorf("инструмент %s: через stdin передаётся только один текст", t.Name)
		}
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
func sourceSlot(name, what string, live []string) slot {
	description := what + ": путь к файлу внутри разрешённых каталогов"
	if len(live) > 0 {
		description += " или живой источник (" + strings.Join(live, "://, ") + "://)"
	}
	return slot{Name: name, Kind: slotSource, Description: description, Required: true}
}

func scenarioSlot() slot {
	return slot{Name: "scenario", Kind: slotFile, Required: true,
		Description: "файл сценария (YAML/JSON с полем service) внутри разрешённых каталогов"}
}

func overridesSlot() slot {
	return slot{Name: "overrides", Kind: slotOverrides,
		Description: "переопределения параметров сценария: {\"параметр\": \"значение\"}"}
}

func scriptSlot() slot {
	return slot{Name: "script", Kind: slotScript, Required: true,
		Description: "текст сценария на языке алиасов ochan; встроенные функции и алиасы перечисляет инструмент aliases"}
}

func promptSlot() slot {
	return slot{Name: "prompt", Kind: slotText, Required: true,
		Description: "текст формального промпта; язык описан в протоколе (инструмент protocol). " +
			"Отчёт report: дописывается к промпту и проверяется вместе с ним"}
}

func nameSlot() slot {
	return slot{Name: "name", Kind: slotName, Flag: "name",
		Description: "имя сценария, например поднять_лимит.ochan: оно попадёт в сообщения об ошибках, план и заявку"}
}

func docSlot() slot {
	return slot{Name: "doc", Kind: slotNewDoc, Flag: "doc",
		Description: "выгрузить заявку в новый файл .docx; существующий файл не перезаписывается"}
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
		return slotError("changes", "%s содержит «=», и ochan примет его за переопределение "+
			"параметра сценария; уберите «=» из пути или параметров ссылки", second)
	}
	return nil
}

// buildTools собирает таблицу инструментов.
func buildTools(live []string) []tool {
	return []tool{
		{
			Name:        "diff",
			Title:       "Сравнить две конфигурации",
			Description: "Сравнивает конфигурации по смыслу: 8GB и 8192MB, 1s и 1000ms считаются равными. Ничего не записывает.",
			Command:     "diff",
			Fixed:       []string{"--fast"},
			Slots: []slot{
				sourceSlot("left", "первая конфигурация", live),
				sourceSlot("right", "вторая конфигурация", live),
				{Name: "include_defaults", Kind: slotSwitch, Flag: "all",
					Description: "для живых источников включать параметры со значением по умолчанию"},
			},
		},
		{
			Name:        "validate",
			Title:       "Проверить конфигурацию",
			Description: "Проверяет, что конфигурация разбирается и содержит параметры.",
			Command:     "validate",
			Slots:       []slot{sourceSlot("source", "конфигурация", live)},
		},
		{
			Name:        "find",
			Title:       "Найти параметр",
			Description: "Ищет параметр во всех секциях конфигурации и показывает его значения.",
			Command:     "find",
			Slots: []slot{
				sourceSlot("source", "конфигурация", live),
				{Name: "param", Kind: slotParam, Required: true, Description: "имя параметра"},
			},
		},
		{
			Name:        "stats",
			Title:       "Статистика конфигурации",
			Description: "Показывает число секций и параметров, а также замечания источника.",
			Command:     "stats",
			Slots:       []slot{sourceSlot("source", "конфигурация", live)},
		},
		{
			Name:        "print",
			Title:       "Показать файл",
			Description: "Выводит содержимое файла как есть.",
			Command:     "print",
			Slots: []slot{{Name: "file", Kind: slotFile, Required: true,
				Description: "файл внутри разрешённых каталогов"}},
		},
		{
			Name:        "plan_config",
			Title:       "План правки конфигурации",
			Description: "Показывает, какие параметры изменила бы правка base значениями из changes. Ничего не меняет.",
			Command:     "plan",
			Slots: []slot{
				sourceSlot("base", "конфигурация, которую правят", live),
				sourceSlot("changes", "конфигурация с новыми значениями", live),
			},
			check: changesNotOverride,
		},
		{
			Name:  "plan_scenario",
			Title: "План сценария",
			Description: "Показывает, какие операции выполнил бы сценарий и над чем. Изменяющие запросы " +
				"в режиме плана запрещены на уровне клиента ochan.",
			Command: "plan",
			Slots:   []slot{scenarioSlot(), overridesSlot()},
			Remote:  true,
		},
		{
			Name:        "services",
			Title:       "Сервисы сценариев",
			Description: "Перечисляет сервисы, которые можно указать в поле service сценария, и их исполнителей.",
			Command:     "services",
		},
		{
			Name:        "aliases",
			Title:       "Алиасы и встроенные функции",
			Description: "Перечисляет алиасы и встроенные функции языка ochan: библиотеку навыков. Только список, без выполнения.",
			Command:     "aliases",
			Slots: []slot{{Name: "package", Kind: slotPackage,
				Description: "показать только этот пакет алиасов"}},
		},
		{
			Name:        "modules",
			Title:       "Установленные модули",
			Description: "Перечисляет установленные модули команд ochan.",
			Command:     "modules",
		},
		{
			Name:        "history",
			Title:       "Журнал операций",
			Description: "Без id перечисляет операции от новых к старым; с id показывает подробности записи.",
			Command:     "history",
			Slots: []slot{
				{Name: "id", Kind: slotID, Description: "идентификатор записи или его начало"},
				{Name: "limit", Kind: slotLimit, Flag: "limit", Description: "сколько записей показать"},
			},
		},
		{
			Name:        "requests",
			Title:       "Заявки",
			Description: "Без id перечисляет заявки; с id показывает карточку заявки с планом.",
			Command:     "requests",
			Slots: []slot{{Name: "id", Kind: slotID,
				Description: "идентификатор заявки или его начало"}},
		},
		{
			Name:  "request_config",
			Title: "Заявка на правку конфигурации",
			Description: "Создаёт заявку в состоянии «ждёт решения» на правку base значениями из changes. " +
				"Сама заявка ничего не меняет: согласует и выполняет её человек.",
			Command: "request",
			Slots: []slot{
				sourceSlot("base", "конфигурация, которую правят", live),
				sourceSlot("changes", "конфигурация с новыми значениями", live),
				docSlot(),
			},
			Writes: true,
			check:  changesNotOverride,
		},
		{
			Name:  "request_scenario",
			Title: "Заявка на выполнение сценария",
			Description: "Создаёт заявку в состоянии «ждёт решения» на выполнение сценария. В заявке " +
				"сохраняется текст сценария. Сама заявка ничего не выполняет: согласует и выполняет её человек.",
			Command: "request",
			Slots:   []slot{scenarioSlot(), overridesSlot(), docSlot()},
			Writes:  true,
			Remote:  true,
		},
		{
			Name:  "protocol",
			Title: "Протокол",
			Description: "Протокол работы с ochan: кто модель здесь, язык формального промпта, отказы и цикл " +
				"работы. Его же сервер отдаёт в instructions при подключении; инструмент — для клиентов, " +
				"которые instructions не показывают модели.",
			Command: "fp",
			Fixed:   []string{"protocol"},
		},
		{
			Name:  "check_prompt",
			Title: "Проверить формальный промпт",
			Description: "Детерминированная проверка до модели: текст принадлежит языку (⊥syn), слова однозначны " +
				"(⊥ambig), знания хватает — Req(P) ⊆ K (⊥undef), жёсткие правила совместны (⊥conflict). " +
				"Скиллы сверяются с библиотекой пакетов алиасов. С разделом report: проверяет и отчёт.",
			Command: "fp",
			Fixed:   []string{"check"},
			Slots:   []slot{promptSlot(), nameSlot()},
		},
		{
			Name:  "check_script",
			Title: "Проверить сценарий",
			Description: "Проверяет сценарий на языке алиасов, ничего не выполняя: синтаксис, неизвестные " +
				"имена и функции, число аргументов, break и return не на своём месте, явно неверные типы. " +
				"Находит ошибки и в ветках, которые при выполнении не сработали бы. Сообщает все ошибки сразу.",
			Command: "check",
			Slots:   []slot{scriptSlot(), nameSlot()},
		},
		{
			Name:  "plan_script",
			Title: "План сценария на языке алиасов",
			Description: "Проверяет сценарий и выполняет его в режиме плана: write и save ничего не записывают, " +
				"а план показывает, какие файлы и параметры изменились бы. Сценарий работает в песочнице " +
				"сервера: файлы только внутри разрешённых каталогов, переменные окружения и живые источники — " +
				"только разрешённые. Выводит то, что напечатал сценарий, и план.",
			Command:   "plan",
			Fixed:     []string{"--script"},
			Sandboxed: true,
			Slots:     []slot{scriptSlot(), nameSlot()},
		},
		{
			Name:  "request_script",
			Title: "Заявка на выполнение сценария на языке алиасов",
			Description: "Проверяет сценарий, строит план в песочнице сервера и создаёт заявку в состоянии " +
				"«ждёт решения». В заявке сохраняются текст сценария, план и песочница. Сама заявка ничего " +
				"не выполняет: согласует и выполняет её человек, и перед выполнением план строится заново " +
				"и должен совпасть с согласованным.",
			Command:   "request",
			Fixed:     []string{"--script"},
			Sandboxed: true,
			Slots:     []slot{scriptSlot(), nameSlot(), docSlot()},
			Writes:    true,
		},
		{
			Name:        "task_status",
			Title:       "Статус задачи",
			Description: "Показывает статус и журнал задачи во внешней системе исполнителя.",
			Command:     "status",
			Slots: []slot{{Name: "task_id", Kind: slotTaskID, Required: true,
				Description: "идентификатор задачи"}},
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
		return nil, "", fmt.Errorf("неизвестные параметры: %s", strings.Join(unknown, ", "))
	}

	command := append([]string{t.Command}, t.Fixed...)
	if t.Sandboxed {
		command = append(command, box.ochanFlags()...)
	}
	var (
		positional []string
		stdin      string
	)

	for _, s := range t.Slots {
		raw, present := args[s.Name]
		if !present || raw == nil {
			if s.Required {
				return nil, "", slotError(s.Name, "обязательный параметр не задан")
			}
			continue
		}

		if stdinSlot(s.Kind) {
			text, err := scriptText(s.Name, raw)
			if err != nil {
				return nil, "", err
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
