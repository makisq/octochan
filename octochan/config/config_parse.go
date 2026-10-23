package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ConfigMap — разобранный конфигурационный файл: секция -> ключ -> значение.
// Параметры вне секций складываются в секцию с пустым именем.
type ConfigMap map[string]map[string]string

// DiffStatus описывает вид расхождения.
type DiffStatus string

const (
	DiffModified  DiffStatus = "Modified"
	DiffOnlyLeft  DiffStatus = "OnlyInLeft"
	DiffOnlyRight DiffStatus = "OnlyInRight"
)

// DiffEntry — одно расхождение между конфигами.
//
// Секция и ключ хранятся раздельно. Прежняя схема склеивала их в строку
// "section.key" и потом разбирала обратно по первой точке, из-за чего
// секция "a.b" с ключом "c" читалась как секция "a" с ключом "b.c".
type DiffEntry struct {
	Section     string     `json:"section"`
	Key         string     `json:"key"`
	Left        string     `json:"left"`
	Right       string     `json:"right"`
	LeftExists  bool       `json:"left_exists"`
	RightExists bool       `json:"right_exists"`
	Status      DiffStatus `json:"status"`
}

// Param возвращает человекочитаемое имя параметра.
func (e DiffEntry) Param() string {
	if e.Section == "" {
		return e.Key
	}
	return e.Section + "." + e.Key
}

// Diff — результат сравнения двух конфигов.
type Diff struct {
	LeftName  string      `json:"left_name"`
	RightName string      `json:"right_name"`
	Entries   []DiffEntry `json:"entries"`
}

// Len возвращает количество расхождений.
func (d *Diff) Len() int {
	if d == nil {
		return 0
	}
	return len(d.Entries)
}

// Any сообщает, удовлетворяет ли хотя бы один элемент предикату.
func Any[T any](slice []T, predicate func(T) bool) bool {
	for _, v := range slice {
		if predicate(v) {
			return true
		}
	}
	return false
}

// stripComment убирает комментарий, начинающийся с '#', уважая кавычки.
//
// Прежняя реализация применяла регулярку `\s*#.*$` ко всей строке и резала
// значения вида log_line_prefix = '%m [%p] #' или пароли с решёткой.
func stripComment(line string) string {
	var (
		inSingle bool
		inDouble bool
	)
	for i, r := range line {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return line[:i]
			}
		}
	}
	return line
}

// unquote снимает парные кавычки со значения.
// Удвоенная одинарная кавычка внутри — экранирование, как в postgresql.conf.
func unquote(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if first == '\'' && last == '\'' {
			return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
		}
		if first == '"' && last == '"' {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// utf8BOM встречается в файлах, сохранённых редакторами под Windows.
// Без его удаления первый ключ читается вместе с меткой порядка байт
// и попадает в diff как отдельный, «отсутствующий с другой стороны» параметр.
const utf8BOM = "\uFEFF"

// ParseConfig разбирает ini/conf-подобное содержимое.
func ParseConfig(content string) (ConfigMap, error) {
	content = strings.TrimPrefix(content, utf8BOM)

	config := make(ConfigMap)
	currentSection := ""

	for lineNo, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(stripComment(rawLine))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.TrimSpace(line[1 : len(line)-1])
			if _, exists := config[currentSection]; !exists {
				config[currentSection] = make(map[string]string)
			}
			continue
		}

		sepIndex := strings.IndexAny(line, "=:")
		if sepIndex <= 0 {
			// Строки без разделителя игнорируем: в postgresql.conf так
			// выглядят include-директивы и мусор после правок.
			continue
		}

		key := strings.TrimSpace(line[:sepIndex])
		if key == "" {
			return nil, fmt.Errorf("строка %d: пустое имя параметра", lineNo+1)
		}

		value := unquote(strings.TrimSpace(line[sepIndex+1:]))

		if _, exists := config[currentSection]; !exists {
			config[currentSection] = make(map[string]string)
		}
		config[currentSection][key] = value
	}

	return config, nil
}

var boolMapping = map[string]string{
	"yes":      "on",
	"no":       "off",
	"true":     "on",
	"false":    "off",
	"on":       "on",
	"off":      "off",
	"enable":   "on",
	"disable":  "off",
	"enabled":  "on",
	"disabled": "off",
}

var sizeUnits = map[string]float64{
	"b":  1,
	"kb": 1024, "k": 1024,
	"mb": math.Pow(1024, 2), "m": math.Pow(1024, 2),
	"gb": math.Pow(1024, 3), "g": math.Pow(1024, 3),
	"tb": math.Pow(1024, 4), "t": math.Pow(1024, 4),
	"pb": math.Pow(1024, 5), "p": math.Pow(1024, 5),
}

// durationUnits в микросекундах — базовая единица postgresql.conf.
var durationUnits = map[string]float64{
	"us":  1,
	"ms":  1_000,
	"s":   1_000_000,
	"min": 60 * 1_000_000,
	"h":   3_600 * 1_000_000,
	"d":   86_400 * 1_000_000,
}

// splitNumberUnit разбивает "8GB" на 8 и "gb".
func splitNumberUnit(s string) (float64, string, bool) {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == '-' || s[i] == '+') {
		i++
	}
	if i == 0 {
		return 0, "", false
	}

	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, "", false
	}

	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	return num, unit, true
}

// NormalizeValue приводит значение к канонической форме для сравнения.
//
// Результат предназначен только для сравнения и наружу не показывается —
// в отчёте выводятся исходные значения.
//
// Отличия от прежней версии:
//   - "1"/"0" больше не считаются синонимами on/off: max_connections = 1
//     и max_connections = on это разные вещи;
//   - размеры сводятся к точному числу байт, а не к округлённому "8.0MB",
//     поэтому 8MB и 8192kB наконец равны;
//   - добавлены единицы времени (us/ms/s/min/h/d);
//   - регистр свободных строк сохраняется, чтобы /var/lib/PG и /var/lib/pg
//     не выглядели одинаковыми.
func NormalizeValue(value string) string {
	clean := strings.TrimSpace(unquote(strings.TrimSpace(value)))
	if clean == "" {
		return ""
	}

	lower := strings.ToLower(clean)

	if mapped, ok := boolMapping[lower]; ok {
		return "bool:" + mapped
	}

	if num, unit, ok := splitNumberUnit(lower); ok {
		if unit == "" {
			if num == math.Trunc(num) {
				return "num:" + strconv.FormatInt(int64(num), 10)
			}
			return "num:" + strconv.FormatFloat(num, 'f', -1, 64)
		}
		if multiplier, found := sizeUnits[unit]; found {
			return "size:" + strconv.FormatInt(int64(num*multiplier), 10)
		}
		if multiplier, found := durationUnits[unit]; found {
			return "dur:" + strconv.FormatInt(int64(num*multiplier), 10)
		}
	}

	return "str:" + clean
}

// CompareConfigs сравнивает два конфига и возвращает отсортированный diff.
func CompareConfigs(left, right ConfigMap, leftName, rightName string) *Diff {
	type paramKey struct {
		section string
		key     string
	}

	seen := make(map[paramKey]struct{})
	for section, params := range left {
		for key := range params {
			seen[paramKey{section, key}] = struct{}{}
		}
	}
	for section, params := range right {
		for key := range params {
			seen[paramKey{section, key}] = struct{}{}
		}
	}

	keys := make([]paramKey, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}

	diff := &Diff{LeftName: leftName, RightName: rightName}

	for _, k := range keys {
		leftVal, leftExists := left[k.section][k.key]
		rightVal, rightExists := right[k.section][k.key]

		// Параметр, присутствующий с пустым значением, и отсутствующий
		// параметр — это разные вещи, поэтому наличие сверяется отдельно.
		if leftExists == rightExists && NormalizeValue(leftVal) == NormalizeValue(rightVal) {
			continue
		}

		entry := DiffEntry{
			Section:     k.section,
			Key:         k.key,
			Left:        leftVal,
			Right:       rightVal,
			LeftExists:  leftExists,
			RightExists: rightExists,
		}
		switch {
		case leftExists && rightExists:
			entry.Status = DiffModified
		case leftExists:
			entry.Status = DiffOnlyLeft
		default:
			entry.Status = DiffOnlyRight
		}

		diff.Entries = append(diff.Entries, entry)
	}

	sortDiffEntries(diff.Entries)
	return diff
}

// statusRank задаёт порядок групп в отчёте.
func statusRank(s DiffStatus) int {
	switch s {
	case DiffModified:
		return 0
	case DiffOnlyLeft:
		return 1
	case DiffOnlyRight:
		return 2
	default:
		return 3
	}
}

// sortDiffEntries сортирует расхождения.
//
// Прежний компаратор не был транзитивным (сравнивал статусы попарно
// несогласованными правилами), из-за чего порядок строк в отчёте был
// непредсказуемым от запуска к запуску.
func sortDiffEntries(entries []DiffEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := statusRank(entries[i].Status), statusRank(entries[j].Status)
		if ri != rj {
			return ri < rj
		}
		if entries[i].Section != entries[j].Section {
			return entries[i].Section < entries[j].Section
		}
		return entries[i].Key < entries[j].Key
	})
}

// truncateRunes обрезает строку по рунам, а не по байтам.
// Байтовая обрезка ломала UTF-8 в русских значениях.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

// padRunes дополняет строку пробелами до ширины в рунах.
// fmt-глагол %-50s считает байты, поэтому кириллица разъезжала колонки.
func padRunes(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

const (
	paramColumn  = 50
	valueColumn  = 60
	statusColumn = 15
)

// SaveDiffToFile сохраняет отчёт о расхождениях в текстовый файл.
func SaveDiffToFile(diff *Diff, outputFile string) error {
	if diff == nil {
		return fmt.Errorf("нечего сохранять: diff не рассчитан")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Comparison report: %s vs %s\n", diff.LeftName, diff.RightName)
	fmt.Fprintf(&b, "Generated at: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))
	b.WriteString(padRunes("Parameter", paramColumn) + " | " +
		padRunes(diff.LeftName, valueColumn) + " | " +
		padRunes(diff.RightName, valueColumn) + " | " +
		padRunes("Status", statusColumn) + "\n")
	b.WriteString(strings.Repeat("-", paramColumn+valueColumn*2+statusColumn+9) + "\n")

	for _, entry := range diff.Entries {
		left, right := entry.Left, entry.Right
		if !entry.LeftExists {
			left = "N/A"
		}
		if !entry.RightExists {
			right = "N/A"
		}

		b.WriteString(padRunes(truncateRunes(entry.Param(), paramColumn), paramColumn) + " | " +
			padRunes(truncateRunes(left, valueColumn), valueColumn) + " | " +
			padRunes(truncateRunes(right, valueColumn), valueColumn) + " | " +
			padRunes(string(entry.Status), statusColumn) + "\n")
	}

	if err := os.WriteFile(outputFile, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("не удалось записать отчёт %s: %w", outputFile, err)
	}
	return nil
}

// GetNextReportNumber возвращает следующий свободный номер отчёта.
func GetNextReportNumber(reportDir string) (int, error) {
	files, err := os.ReadDir(reportDir)
	if err != nil {
		return 0, fmt.Errorf("не удалось прочитать директорию отчётов: %w", err)
	}

	const prefix, suffix = "diff_report_", ".txt"
	maxNum := 0

	for _, file := range files {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		num, err := strconv.Atoi(name[len(prefix) : len(name)-len(suffix)])
		if err != nil {
			continue
		}
		if num > maxNum {
			maxNum = num
		}
	}

	return maxNum + 1, nil
}

// quoteIfNeeded возвращает значение в кавычках, если без них оно
// прочитается иначе.
//
// Без этого запись теряла данные: log_line_prefix = '%m [%p] #' сохранялся
// без кавычек, и при следующем чтении всё после решётки отрезалось
// как комментарий.
func quoteIfNeeded(value string) string {
	if value == "" {
		return value
	}

	needsQuotes := strings.ContainsAny(value, "#;") ||
		value != strings.TrimSpace(value) ||
		strings.HasPrefix(value, "'") ||
		strings.HasPrefix(value, `"`)

	if !needsQuotes {
		return value
	}

	// Одинарные кавычки — как в postgresql.conf; внутренние удваиваются.
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// SaveConfig записывает конфиг в файл.
func SaveConfig(config ConfigMap, filePath string) error {
	if err := os.WriteFile(filePath, []byte(FormatConfig(config)), 0o644); err != nil {
		return fmt.Errorf("не удалось записать конфиг %s: %w", filePath, err)
	}
	return nil
}

// FormatConfig возвращает текст, который SaveConfig записал бы в файл.
//
// Порядок теперь детерминированный, а параметры без секции пишутся первыми.
// Прежняя реализация обходила map в случайном порядке, из-за чего секция ""
// могла оказаться после [section] и её ключи при обратном чтении попадали
// в чужую секцию.
func FormatConfig(config ConfigMap) string {
	sections := make([]string, 0, len(config))
	for section := range config {
		sections = append(sections, section)
	}
	sort.Strings(sections) // пустая строка сортируется первой

	var b strings.Builder
	for _, section := range sections {
		if section != "" {
			fmt.Fprintf(&b, "[%s]\n", section)
		}

		keys := make([]string, 0, len(config[section]))
		for key := range config[section] {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, key := range keys {
			fmt.Fprintf(&b, "%s = %s\n", key, quoteIfNeeded(config[section][key]))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// GetScriptDir возвращает директорию исполняемого файла.
func GetScriptDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("не удалось определить директорию исполняемого файла: %w", err)
	}
	return filepath.Dir(exe), nil
}

// ResolveFilePath дополняет относительный путь базовой директорией.
func ResolveFilePath(baseDir, filePath string) string {
	if filepath.IsAbs(filePath) {
		return filePath
	}
	return filepath.Join(baseDir, filePath)
}

// FileExists сообщает, существует ли обычный файл по пути path.
// Прежняя версия возвращала true при ошибке доступа.
func FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ReadFile читает файл целиком.
func ReadFile(filePath string) (string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	return string(content), nil
}
