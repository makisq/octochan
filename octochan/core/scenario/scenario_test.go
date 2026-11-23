package scenario

import (
	"testing"
)

func TestParseScenarioData(t *testing.T) {
	t.Run("YAML со списком targets", func(t *testing.T) {
		data := []byte(`
service: psql_tuning_params_se
parameters:
  role: standalone
targets:
  - svm_ci: CI-1
  - svm_ci: ["CI-2", "CI-3"]
`)
		s, err := Parse("<тест>", data)
		if err != nil {
			t.Fatalf("ParseScenarioData: %v", err)
		}
		if s.Service != "psql_tuning_params_se" {
			t.Errorf("service = %q", s.Service)
		}
		if len(s.Targets) != 2 {
			t.Fatalf("получено %d targets, ожидалось 2", len(s.Targets))
		}
		if got := s.Targets[0].GetCIs(); len(got) != 1 || got[0] != "CI-1" {
			t.Errorf("target[0].GetCIs() = %v", got)
		}
		if got := s.Targets[1].GetCIs(); len(got) != 2 {
			t.Errorf("target[1].GetCIs() = %v, ожидалось 2 элемента", got)
		}
	})

	t.Run("JSON с одиночным targets", func(t *testing.T) {
		data := []byte(`{"service":"s","targets":{"svm_ci":"CI-9","ip":"10.0.0.1"}}`)
		s, err := Parse("<тест>", data)
		if err != nil {
			t.Fatalf("ParseScenarioData: %v", err)
		}
		if len(s.Targets) != 1 || s.Targets[0].SVMCI != "CI-9" || s.Targets[0].IP != "10.0.0.1" {
			t.Errorf("targets разобраны неверно: %+v", s.Targets)
		}
	})

	t.Run("без service", func(t *testing.T) {
		if _, err := Parse("<тест>", []byte(`targets: [{svm_ci: CI-1}]`)); err == nil {
			t.Error("ожидалась ошибка об отсутствующем service")
		}
	})

	t.Run("мусор", func(t *testing.T) {
		if _, err := Parse("<тест>", []byte("\t\x00не конфиг")); err == nil {
			t.Error("ожидалась ошибка разбора")
		}
	})
}

// «ключ: ?» без кавычек — общая строка, которую ждут: сценарий разбирается.
func TestBarePlaceholder(t *testing.T) {
	s, err := Parse("c.yaml", []byte("service: http\ntargets:\n  - ip: 10.0.0.1\nparameters:\n  string_i: ?\n  note: \"a: ?\"  # строка, не трогать\n  tail: ?   # комментарий\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Parameters["string_i"] != "?" || s.Parameters["tail"] != "?" || s.Parameters["note"] != "a: ?" {
		t.Fatalf("параметры: %v", s.Parameters)
	}
}
