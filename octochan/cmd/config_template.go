package cmd

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Шаблон config.yaml: им ochan создаёт конфиг, которого ещё нет.
//
// Раньше новый конфиг был либо выгрузкой всех ~110 умолчаний без единого
// пояснения (первый запуск ochan), либо парой ключей, которые записала
// установка (ochan setup), — и в обоих случаях было не понять, что тут
// настраивают. Шаблон — главные настройки по разделам, у каждой строка
// «что это»; значение по умолчанию закомментировано, и снять # — значит
// задать. Заданное установкой — без #. Умолчания в файл не выгружаются:
// новое умолчание следующей версии дойдёт и до этого конфига, а пути
// рабочего каталога не замёрзнут при переносе папки.

// configEntry — настройка в шаблоне: ключ через точку и что она задаёт.
// example — значение-пример вместо умолчания: у настройки его нет (адрес
// API, источник инвентаря) или оно — путь этой машины.
type configEntry struct {
	key     string
	comment string
	example any
}

// configSection — раздел шаблона: заголовок и настройки одного ключа
// верхнего уровня (у каждого раздела свой — в YAML ключ не повторить).
type configSection struct {
	title   string
	entries []configEntry
}

var configSections = []configSection{
	{"Язык справки и сообщений", []configEntry{
		{"lang", "ru, eng или язык из модуля (ochan lang)", nil},
	}},
	{"Где лежит то, чем пользуется ochan (ochan paths); пусто — в рабочем каталоге", []configEntry{
		{"paths.modules", "модули — туда, где политика хоста разрешает запуск программ", nil},
		{"paths.logs", "журнал ochan и его архивы", nil},
		{"paths.reports", "отчёты ochan diff", nil},
		{"paths.registry_cache", "клоны git-реестров пакетов", nil},
		{"paths.warn_open", "предупреждать при запуске, что каталог открыт для чужой записи", nil},
	}},
	{"Исполнители и почта: общие умолчания", []configEntry{
		{"defaults.api_url", "адрес API по умолчанию для исполнителей (свой — executors.<имя>.api_url)", nil},
		{"defaults.http_timeout", "предел одного запроса исполнителя", nil},
		{"defaults.max_parallel_tasks", "сколько целей сценария обрабатывать одновременно", nil},
		{"defaults.smtp_host", "почтовый сервер для заявок, эскалаций и отчётов; пароль — OCTOCHAN_SMTP_PASSWORD", nil},
		{"defaults.smtp_port", "порт почтового сервера", nil},
		{"defaults.smtp_from", "адрес отправителя", nil},
		{"defaults.smtp_to", "кому письма по умолчанию", nil},
		{"defaults.smtp_starttls", "шифровать соединение с почтовым сервером", nil},
		{"defaults.token_timer_enabled", "напоминать в оболочке о смене токена (ochan timer on)", nil},
	}},
	{"Исполнители: своё у каждого, под его именем; токен — ochan auth --executor <имя>", []configEntry{
		{"executors.http.api_url", "исполнитель ядра http: адрес API узла", "https://node.example.internal/api"},
		{"executors.http.auth_scheme", "схема заголовка Authorization", "Bearer"},
	}},
	{"Инвентарь: откуда брать хосты для --limit и inventory()", []configEntry{
		{"inventory.sources.main", "имя источника и путь к YAML-файлу", "/srv/inventory/hosts.yaml"},
	}},
	{"Оболочка", []configEntry{
		{"shell.logo", "логотип при входе", nil},
		{"shell.history_size", "сколько команд помнить между запусками", nil},
	}},
	{"Формальный промпт", []configEntry{
		{"fp.mode", "formal — модели ставят задачу формальным промптом, natural — обычным языком", nil},
	}},
	{"Нейросеть", []configEntry{
		{"llm.mode", "protocol, natural или embedded (своя модель); пусто — по fp.mode", nil},
		{"llm.model", "каталог своей модели (ochan llm init --pretrained --out …)", nil},
		{"llm.chat.provider", "разговор из оболочки (ask): anthropic, gigachat или openai; ключ — ochan auth --executor llm", nil},
		{"llm.chat.model", "модель поставщика; пусто — его модель по умолчанию", nil},
		{"llm.chat.root", "папки, в которых модель читает файлы; пусто — папка, из которой запущен ochan", nil},
	}},
	{"Почта агентов и людей", []configEntry{
		{"mail.operator", "группа «оператор» для ochan mail send --to operator", nil},
		{"mail.groups.dba", "группа адресов: --to dba", []string{"dba@example.internal"}},
	}},
	{"Инциденты HA-AI", []configEntry{
		{"incidents.engineers", "кому эскалация; пусто — defaults.smtp_to", []string{"oncall@example.internal"}},
		{"incidents.report.every", "отчёт со статистикой на почту: 24h, 7d; пусто — не слать", nil},
	}},
	{"Сервер: общий оркестратор (ochan serve)", []configEntry{
		{"server.listen", "адрес, на котором ochan serve принимает клиентов", nil},
	}},
	{"Подключение к общему оркестратору", []configEntry{
		{"remote.address", "адрес сервера для ochan remote без адреса", nil},
	}},
	{"Песочница ОС для --sandbox", []configEntry{
		{"sandbox.strict", "отказываться выполнять, если ядро не даёт полной изоляции", nil},
	}},
}

const configHeader = `# config.yaml ochan.
#
# Строка с # — пояснение или настройка, которая сейчас по умолчанию:
# снимите # с нужной (и с заголовка её раздела), чтобы задать своё.
# Отступ — два пробела на уровень.
#
# Здесь — главное. Все настройки с пояснениями — docs/reference/config.md
# в репозитории ochan. Где что лежит — ochan paths. Пароли и токены сюда не
# пишутся: ochan auth --executor <имя>, почта — OCTOCHAN_SMTP_PASSWORD.
`

// renderConfigTemplate — текст шаблона; set — настройки, которые надо
// задать (без #): ключ через точку → значение.
func renderConfigTemplate(set map[string]any) (string, error) {
	var b strings.Builder
	b.WriteString(configHeader)
	used := map[string]bool{}
	for _, section := range configSections {
		b.WriteString("\n# " + section.title + "\n")
		// Заголовки (paths:, llm:, chat:) — без #, если под ними что-то
		// задано.
		active := func(prefix string) bool {
			for key := range set {
				if key == prefix || strings.HasPrefix(key, prefix+".") {
					return true
				}
			}
			return false
		}
		var open []string
		for _, entry := range section.entries {
			parts := strings.Split(entry.key, ".")
			common := 0
			for common < len(open) && common < len(parts)-1 && open[common] == parts[common] {
				common++
			}
			open = open[:common]
			for depth := common; depth < len(parts)-1; depth++ {
				open = append(open, parts[depth])
				prefix := strings.Join(open, ".")
				line := strings.Repeat("  ", depth) + parts[depth] + ":"
				if !active(prefix) {
					line = "# " + line
				}
				b.WriteString(line + "\n")
			}
			indent := strings.Repeat("  ", len(parts)-1)
			mark := "# "
			if active(strings.Join(parts[:len(parts)-1], ".")) || len(parts) == 1 {
				mark = ""
			}
			fmt.Fprintf(&b, "%s%s# %s\n", mark, indent, entry.comment)
			value, isSet := set[entry.key]
			if isSet {
				used[entry.key] = true
			} else if entry.example != nil {
				value = entry.example
			} else {
				value = viper.Get(entry.key)
			}
			text, err := yamlInline(value)
			if err != nil {
				return "", fmt.Errorf("%s: %w", entry.key, err)
			}
			line := fmt.Sprintf("%s%s: %s", indent, parts[len(parts)-1], text)
			switch {
			case isSet:
			case mark == "":
				// Раздел задан — # после отступа: строка остаётся на месте.
				line = fmt.Sprintf("%s# %s: %s", indent, parts[len(parts)-1], text)
			default:
				line = "# " + line
			}
			b.WriteString(line + "\n")
		}
	}
	// Заданное, чего в шаблоне нет, — в конце, чтобы не потерялось.
	var rest []string
	for key := range set {
		if !used[key] {
			rest = append(rest, key)
		}
	}
	if len(rest) > 0 {
		extra := map[string]any{}
		for _, key := range rest {
			putDotted(extra, key, set[key])
		}
		data, err := yaml.Marshal(extra)
		if err != nil {
			return "", err
		}
		b.WriteString("\n# Задано установкой\n")
		b.Write(data)
	}
	return b.String(), nil
}

// yamlInline — значение одной строкой YAML: "" , [] , [a, b], 30s.
func yamlInline(value any) (string, error) {
	if value == nil {
		return `""`, nil
	}
	node := &yaml.Node{}
	if err := node.Encode(value); err != nil {
		return "", err
	}
	if node.Kind == yaml.SequenceNode || node.Kind == yaml.MappingNode {
		node.Style = yaml.FlowStyle
	}
	data, err := yaml.Marshal(node)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		text = `""`
	}
	return text, nil
}

func putDotted(m map[string]any, key string, value any) {
	parts := strings.Split(key, ".")
	for _, part := range parts[:len(parts)-1] {
		next, _ := m[part].(map[string]any)
		if next == nil {
			next = map[string]any{}
			m[part] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = value
}

// writeConfigTemplate создаёт config.yaml из шаблона; set — заданное.
func writeConfigTemplate(path string, set map[string]any) error {
	text, err := renderConfigTemplate(set)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o600)
}

// setConfigValues задаёт настройки в config.yaml и не трогает остальное:
// комментарии, порядок, чужие ключи. Файла нет — он создаётся шаблоном.
// Раньше для этого выгружали все настройки (viper.WriteConfig): пропадали
// пояснения, а умолчания замерзали в файле.
func setConfigValues(path string, set map[string]any) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return writeConfigTemplate(path, set)
	}
	if err != nil {
		return err
	}
	// Файл из шаблона: настройка меняется в своей строке — снимается # с
	// неё и с заголовков её раздела. Комментарии, порядок и правки руками
	// остаются как были.
	if text, ok := editTemplateText(string(data), set); ok {
		if _, err := parseYAML(text); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return os.WriteFile(path, []byte(text), 0o600)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind == 0 {
		// Пустой файл или одни комментарии.
		doc = yaml.Node{Kind: yaml.DocumentNode, HeadComment: doc.HeadComment,
			Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: верхний уровень — не словарь настроек", path)
	}
	for _, key := range sortedPaths(set) {
		value := &yaml.Node{}
		if err := value.Encode(set[key]); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if err := setNode(doc.Content[0], strings.Split(key, "."), value); err != nil {
			return fmt.Errorf("%s: %s: %w", path, key, err)
		}
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o600)
}

func setNode(mapping *yaml.Node, parts []string, value *yaml.Node) error {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != parts[0] {
			continue
		}
		if len(parts) == 1 {
			value.HeadComment, value.LineComment = mapping.Content[i+1].HeadComment, mapping.Content[i+1].LineComment
			mapping.Content[i+1] = value
			return nil
		}
		child := mapping.Content[i+1]
		if child.Kind != yaml.MappingNode {
			if child.Tag != "!!null" {
				return fmt.Errorf("%s — не раздел, а значение", parts[0])
			}
			*child = yaml.Node{Kind: yaml.MappingNode, HeadComment: child.HeadComment}
		}
		return setNode(child, parts[1:], value)
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Value: parts[0]}
	if len(parts) == 1 {
		mapping.Content = append(mapping.Content, key, value)
		return nil
	}
	child := &yaml.Node{Kind: yaml.MappingNode}
	mapping.Content = append(mapping.Content, key, child)
	return setNode(child, parts[1:], value)
}

func parseYAML(text string) (map[string]any, error) {
	out := map[string]any{}
	err := yaml.Unmarshal([]byte(text), &out)
	return out, err
}

// editTemplateText задаёт настройки в тексте конфига, сделанного по
// шаблону: находит строку настройки (с # или без) под её заголовками и
// ставит значение. false — какой-то строки нет (файл писали не по шаблону
// или её удалили): тогда правка идёт через разбор YAML.
func editTemplateText(text string, set map[string]any) (string, bool) {
	lines := strings.Split(text, "\n")
	for _, key := range sortedPaths(set) {
		value, err := yamlInline(set[key])
		if err != nil {
			return "", false
		}
		parts := strings.Split(key, ".")
		at := 0
		var headers []int
		for depth, part := range parts {
			indent := strings.Repeat("  ", depth)
			leaf := depth == len(parts)-1
			found := -1
			for i := at; i < len(lines); i++ {
				line := lines[i]
				body, commented := uncommentLine(line, indent)
				if !strings.HasPrefix(body, indent+part+":") {
					// Раздел кончился: строка с меньшим отступом.
					if depth > 0 && i > at && !commented && strings.TrimSpace(line) != "" &&
						!strings.HasPrefix(line, indent) && !strings.HasPrefix(strings.TrimSpace(line), "#") {
						break
					}
					continue
				}
				rest := strings.TrimSpace(strings.TrimPrefix(body, indent+part+":"))
				if leaf || rest == "" {
					found = i
					break
				}
			}
			if found < 0 {
				// Настройки нет в шаблоне: она встаёт под свой заголовок
				// вместе с недостающими; нет и раздела — в конец файла.
				var add []string
				for d := depth; d < len(parts)-1; d++ {
					add = append(add, strings.Repeat("  ", d)+parts[d]+":")
				}
				add = append(add, strings.Repeat("  ", len(parts)-1)+parts[len(parts)-1]+": "+value)
				pos := at
				if depth == 0 {
					for len(lines) > 0 && lines[len(lines)-1] == "" {
						lines = lines[:len(lines)-1]
					}
					add = append([]string{""}, add...)
					add = append(add, "")
					pos = len(lines)
				}
				lines = append(lines[:pos], append(add, lines[pos:]...)...)
				break
			}
			if leaf {
				lines[found] = indent + part + ": " + value
			} else {
				headers = append(headers, found)
			}
			at = found + 1
		}
		for depth, i := range headers {
			// Заголовок мог стать «␣␣# chat:», когда снимали # с раздела выше.
			if _, commented := uncommentLine(lines[i], strings.Repeat("  ", depth)); !commented {
				continue
			}
			lines[i] = strings.Repeat("  ", depth) + parts[depth] + ":"
			// Строки раздела под снятым заголовком — # после отступа, как
			// у заданного раздела в шаблоне: «#   # пояснение» →
			// «  # пояснение», «#   mode: ""» → «  # mode: ""».
			child := len(strings.Repeat("  ", depth+1))
			for j := i + 1; j < len(lines); j++ {
				m := commentedLine.FindStringSubmatch(lines[j])
				if m == nil {
					if strings.TrimSpace(lines[j]) != "" && leadingSpaces(lines[j]) >= child {
						continue // уже задано в этом разделе
					}
					break
				}
				visual := len(m[1]) + len(m[2])
				if visual < child {
					break
				}
				if strings.HasPrefix(m[3], "#") {
					lines[j] = strings.Repeat(" ", visual) + m[3]
				} else {
					lines[j] = strings.Repeat(" ", visual) + "# " + m[3]
				}
			}
		}
	}
	return strings.Join(lines, "\n"), true
}

// uncommentLine — строка без # шаблона: «# ␣␣ключ» (раздел не задан) или
// «␣␣# ключ» (раздел задан). commented — # был.
func uncommentLine(line, indent string) (string, bool) {
	if rest, ok := strings.CutPrefix(line, "# "); ok {
		return rest, true
	}
	if rest, ok := strings.CutPrefix(line, indent+"# "); ok && indent != "" {
		return indent + rest, true
	}
	return line, false
}

// commentedLine — строка с #: отступ до #, отступ после, остальное.
var commentedLine = regexp.MustCompile(`^( *)# ( *)(.*)$`)

func leadingSpaces(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }
