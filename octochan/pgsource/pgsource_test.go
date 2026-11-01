package pgsource

import (
	"strings"
	"testing"

	"ochan/config"
)

func TestFormatSetting(t *testing.T) {
	tests := []struct {
		name    string
		setting string
		unit    string
		want    string
	}{
		// Множитель в единице — главная ловушка pg_settings.
		// shared_buffers сервер отдаёт как 16384 при unit=8kB.
		{"shared_buffers", "16384", "8kB", "131072kB"},
		{"wal_buffers", "512", "8kB", "4096kB"},
		{"единица без множителя", "64", "MB", "64MB"},
		{"байты", "1048576", "B", "1048576B"},
		{"миллисекунды", "30000", "ms", "30000ms"},
		{"секунды", "60", "s", "60s"},
		{"минуты", "5", "min", "5min"},
		{"множитель у времени", "100", "10ms", "1000ms"},

		// Значения без единиц идут как есть.
		{"логическое", "on", "", "on"},
		{"число без единицы", "100", "", "100"},
		{"строка", "logical", "", "logical"},
		{"путь", "$libdir/plugins", "", "$libdir/plugins"},

		// Нечисловое значение с единицей единицу игнорирует.
		{"нечисловое с единицей", "auto", "8kB", "auto"},

		{"минус один", "-1", "8kB", "-8kB"},
		{"ноль", "0", "8kB", "0kB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatSetting(tt.setting, tt.unit); got != tt.want {
				t.Errorf("FormatSetting(%q, %q) = %q, ожидалось %q",
					tt.setting, tt.unit, got, tt.want)
			}
		})
	}
}

func TestFormatSettingMatchesConfigFile(t *testing.T) {
	// Смысл пересчёта: значение живого сервера и значение из postgresql.conf
	// должны сойтись после нормализации. Без множителя единицы они
	// расходились бы в каждой строке.
	cases := []struct {
		setting   string
		unit      string
		inFile    string
		mustEqual bool
	}{
		{"16384", "8kB", "128MB", true},
		{"16384", "8kB", "131072kB", true},
		{"16384", "8kB", "8GB", false},
		{"30000", "ms", "30s", true},
		{"30000", "ms", "30000ms", true},
		{"5", "min", "300s", true},
		{"100", "", "100", true},
	}

	for _, c := range cases {
		live := config.NormalizeValue(FormatSetting(c.setting, c.unit))
		file := config.NormalizeValue(c.inFile)

		if (live == file) != c.mustEqual {
			t.Errorf("setting=%s unit=%s -> %q против файла %q -> %q: совпадение %v, ожидалось %v",
				c.setting, c.unit, live, c.inFile, file, live == file, c.mustEqual)
		}
	}
}

func TestSplitUnit(t *testing.T) {
	cases := map[string]struct {
		multiplier int64
		base       string
	}{
		"8kB":  {8, "kB"},
		"kB":   {1, "kB"},
		"ms":   {1, "ms"},
		"16MB": {16, "MB"},
		"":     {1, ""},
	}

	for unit, want := range cases {
		multiplier, base := splitUnit(unit)
		if multiplier != want.multiplier || base != want.base {
			t.Errorf("splitUnit(%q) = (%d, %q), ожидалось (%d, %q)",
				unit, multiplier, base, want.multiplier, want.base)
		}
	}
}

func TestParseDSNHidesPassword(t *testing.T) {
	// Отображаемое имя источника попадает в отчёты, журнал и сообщения
	// об ошибках, поэтому пароля в нём быть не должно никогда.
	_, display, err := parseDSN("postgres://dba:s3cret-p4ss@db1.example:5432/postgres")
	if err != nil {
		t.Fatalf("parseDSN: %v", err)
	}

	if strings.Contains(display, "s3cret-p4ss") {
		t.Errorf("пароль попал в отображаемое имя: %s", display)
	}

	for _, want := range []string{"dba", "db1.example", "5432", "postgres"} {
		if !strings.Contains(display, want) {
			t.Errorf("в имени источника нет %q: %s", want, display)
		}
	}
}

func TestStripOwnParams(t *testing.T) {
	// Неизвестные параметры pgx отправляет серверу, и тот отвечает ошибкой,
	// поэтому свои параметры надо снять до разбора.
	cleaned, err := stripOwnParams("postgres://u@h:5432/db?sslmode=require&ochan_all=true")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cleaned, "ochan_all") {
		t.Errorf("собственный параметр остался: %s", cleaned)
	}
	if !strings.Contains(cleaned, "sslmode=require") {
		t.Errorf("потерян параметр драйвера: %s", cleaned)
	}

	// Строка в формате ключ=значение не является URL и не должна ломаться.
	keyword := "host=db1 user=dba dbname=postgres"
	if got, err := stripOwnParams(keyword); err != nil || got != keyword {
		t.Errorf("keyword-строка изменилась: %q (%v)", got, err)
	}
}

func TestUnencodedPasswordGivesUsableHint(t *testing.T) {
	// Пароль с кириллицей или спецсимволами не разбирается как часть URL,
	// а сообщение net/url про "invalid userinfo" ничего не объясняет.
	_, _, err := parseDSN("postgres://dba:Секрет@db1:5432/postgres")
	if err == nil {
		t.Fatal("ожидалась ошибка разбора")
	}

	for _, want := range []string{"PGPASSWORD", "pgpass", "percent-encoded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("в подсказке нет %q: %v", want, err)
		}
	}
}

func TestSchemeIsRegistered(t *testing.T) {
	// Пакет регистрирует себя в init: core не знает про PostgreSQL,
	// источник подключается сам.
	schemes := config.RegisteredSourceSchemes()

	var found bool
	for _, scheme := range schemes {
		if scheme == "postgres" {
			found = true
		}
	}
	if !found {
		t.Errorf("схема postgres не зарегистрирована; есть: %v", schemes)
	}
}
