package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStripCommentRespectsQuotes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"обычный комментарий", "port = 5432 # основной", "port = 5432 "},
		{"строка целиком комментарий", "# ничего", ""},
		{"решётка в одинарных кавычках", `log_line_prefix = '%m [%p] #'`, `log_line_prefix = '%m [%p] #'`},
		{"решётка в двойных кавычках", `password = "a#b"`, `password = "a#b"`},
		{"кавычка внутри значения", `x = 'it''s #1'`, `x = 'it''s #1'`},
		{"без комментария", "shared_buffers = 8GB", "shared_buffers = 8GB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripComment(tt.in); got != tt.want {
				t.Errorf("stripComment(%q) = %q, ожидалось %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseConfig(t *testing.T) {
	content := `
# заголовок
global_param = 1

[server]
port = 5432        # комментарий
host = 'local#host'

[server]
extra = yes
`

	cfg, err := ParseConfig(content)
	if err != nil {
		t.Fatalf("ParseConfig вернул ошибку: %v", err)
	}

	if got := cfg[""]["global_param"]; got != "1" {
		t.Errorf("параметр вне секции: получено %q", got)
	}
	if got := cfg["server"]["port"]; got != "5432" {
		t.Errorf("server.port = %q, ожидалось \"5432\"", got)
	}
	// Решётка внутри кавычек не должна съедаться как комментарий.
	if got := cfg["server"]["host"]; got != "local#host" {
		t.Errorf("server.host = %q, ожидалось \"local#host\"", got)
	}
	// Повторное объявление секции дополняет её, а не затирает.
	if got := cfg["server"]["extra"]; got != "yes" {
		t.Errorf("server.extra = %q, ожидалось \"yes\"", got)
	}
}

func TestParseConfigStripsBOM(t *testing.T) {
	// Файл, сохранённый редактором под Windows, начинается с BOM,
	// и без его удаления первый ключ читается как отдельный параметр.
	cfg, err := ParseConfig("\uFEFFshared_buffers = 8GB\n")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := cfg[""]["shared_buffers"]; got != "8GB" {
		t.Errorf("shared_buffers = %q; ключи: %v", got, cfg[""])
	}
}

func TestNormalizeValue(t *testing.T) {
	equal := [][2]string{
		{"8MB", "8192kB"},
		{"1GB", "1024MB"},
		{"on", "true"},
		{"OFF", "no"},
		{"100", " 100 "},
		{"1s", "1000ms"},
		{"1min", "60s"},
		{"'8GB'", "8GB"},
	}
	for _, pair := range equal {
		if NormalizeValue(pair[0]) != NormalizeValue(pair[1]) {
			t.Errorf("%q и %q должны быть равны, получено %q и %q",
				pair[0], pair[1], NormalizeValue(pair[0]), NormalizeValue(pair[1]))
		}
	}

	different := [][2]string{
		// Раньше "1" отображалось в "on", из-за чего max_connections = 1
		// считалось равным max_connections = on.
		{"1", "on"},
		{"0", "off"},
		{"8MB", "8GB"},
		{"/var/lib/PG", "/var/lib/pg"},
		{"100ms", "100s"},
	}
	for _, pair := range different {
		if NormalizeValue(pair[0]) == NormalizeValue(pair[1]) {
			t.Errorf("%q и %q не должны быть равны (оба дали %q)",
				pair[0], pair[1], NormalizeValue(pair[0]))
		}
	}
}

func TestCompareConfigs(t *testing.T) {
	left := ConfigMap{
		"": {
			"shared_buffers":  "8GB",
			"max_connections": "100",
			"only_left":       "1",
		},
	}
	right := ConfigMap{
		"": {
			"shared_buffers":  "8192MB", // то же значение в других единицах
			"max_connections": "200",
			"only_right":      "2",
		},
	}

	diff := CompareConfigs(left, right, "prod", "test")

	byParam := make(map[string]DiffEntry)
	for _, entry := range diff.Entries {
		byParam[entry.Param()] = entry
	}

	if _, found := byParam["shared_buffers"]; found {
		t.Error("8GB и 8192MB не должны попадать в diff")
	}
	if got := byParam["max_connections"].Status; got != DiffModified {
		t.Errorf("max_connections: статус %q, ожидался %q", got, DiffModified)
	}
	if got := byParam["only_left"].Status; got != DiffOnlyLeft {
		t.Errorf("only_left: статус %q, ожидался %q", got, DiffOnlyLeft)
	}
	if got := byParam["only_right"].Status; got != DiffOnlyRight {
		t.Errorf("only_right: статус %q, ожидался %q", got, DiffOnlyRight)
	}

	// Modified должен идти раньше Only*.
	if diff.Entries[0].Status != DiffModified {
		t.Errorf("первым должен идти Modified, получено %q", diff.Entries[0].Status)
	}
}

func TestCompareConfigsSectionKeyNotAmbiguous(t *testing.T) {
	// Секция "a.b" с ключом "c" раньше склеивалась в "a.b.c" и разбиралась
	// обратно как секция "a" с ключом "b.c".
	left := ConfigMap{"a.b": {"c": "1"}}
	right := ConfigMap{"a": {"b.c": "1"}}

	diff := CompareConfigs(left, right, "l", "r")
	if diff.Len() != 2 {
		t.Fatalf("ожидалось 2 расхождения, получено %d: %+v", diff.Len(), diff.Entries)
	}
	for _, entry := range diff.Entries {
		if entry.Status == DiffModified {
			t.Errorf("разные параметры не должны считаться изменением: %+v", entry)
		}
	}
}

func TestSaveConfigKeepsSectionlessKeysFirst(t *testing.T) {
	// Прежняя реализация обходила map в случайном порядке, поэтому секция ""
	// могла попасть после [server] и её ключи при чтении уезжали в чужую секцию.
	cfg := ConfigMap{
		"server": {"port": "5432"},
		"":       {"global": "1"},
		"alpha":  {"x": "2"},
	}

	path := filepath.Join(t.TempDir(), "out.conf")
	if err := SaveConfig(cfg, path); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение результата: %v", err)
	}

	reparsed, err := ParseConfig(string(data))
	if err != nil {
		t.Fatalf("повторный разбор: %v", err)
	}

	if got := reparsed[""]["global"]; got != "1" {
		t.Errorf("параметр без секции потерялся: %q\nфайл:\n%s", got, data)
	}
	if got := reparsed["server"]["port"]; got != "5432" {
		t.Errorf("server.port = %q\nфайл:\n%s", got, data)
	}
	if _, leaked := reparsed["alpha"]["global"]; leaked {
		t.Errorf("global утёк в секцию alpha:\n%s", data)
	}
}

func TestSaveConfigRoundTripKeepsSpecialValues(t *testing.T) {
	// Значение с решёткой сохранялось без кавычек, и при следующем чтении
	// всё после '#' отрезалось как комментарий — patch терял данные.
	cfg := ConfigMap{"": {
		"log_line_prefix": "%m [%p] #",
		"apostrophe":      "it's fine",
		"padded":          " keep spaces ",
		"plain":           "8GB",
	}}

	path := filepath.Join(t.TempDir(), "roundtrip.conf")
	if err := SaveConfig(cfg, path); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := ParseConfig(string(data))
	if err != nil {
		t.Fatalf("повторный разбор: %v", err)
	}

	for key, want := range cfg[""] {
		if got := reparsed[""][key]; got != want {
			t.Errorf("%s: получено %q, ожидалось %q\nфайл:\n%s", key, got, want, data)
		}
	}
}

func TestTruncateRunesKeepsUTF8(t *testing.T) {
	// Байтовая обрезка ломала кириллицу.
	in := "значение-которое-нужно-обрезать"
	got := truncateRunes(in, 10)

	if len([]rune(got)) != 10 {
		t.Errorf("получено %d рун, ожидалось 10: %q", len([]rune(got)), got)
	}
	for _, r := range got {
		if r == '�' {
			t.Errorf("в результате появился битый символ: %q", got)
		}
	}
}

func TestGetNextReportNumber(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"diff_report_1.txt", "diff_report_7.txt", "мусор.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := GetNextReportNumber(dir)
	if err != nil {
		t.Fatalf("GetNextReportNumber: %v", err)
	}
	if got != 8 {
		t.Errorf("получено %d, ожидалось 8", got)
	}
}
