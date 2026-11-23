package pluginhost

import (
	"strings"
	"testing"
)

// Имя настройки становится ключом YAML: точка, пробел, двоеточие или
// повтор сломали бы раздел модуля в config.yaml — такой модуль не грузится.
func TestCheckSettings(t *testing.T) {
	if err := CheckSettings([]Setting{{Key: "api_url"}, {Key: "retry-count"}, {Key: "X1"}}); err != nil {
		t.Fatalf("годные имена отвергнуты: %v", err)
	}
	for _, bad := range [][]Setting{
		{{Key: "api.url"}}, {{Key: "api url"}}, {{Key: "url:"}}, {{Key: ""}}, {{Key: "1st"}},
		{{Key: "a"}, {Key: "a"}},
	} {
		err := CheckSettings(bad)
		if err == nil {
			t.Fatalf("%+v принято", bad)
		}
		if !strings.Contains(err.Error(), "настройка") {
			t.Fatalf("ошибка без имени настройки: %v", err)
		}
	}
}
