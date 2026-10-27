package scenario

import (
	"encoding/json"
	"strings"
	"testing"

	"octochan/inventory"
)

// Цель — свободный набор полей: исполнитель получает их как есть.
func TestFreeTargetFields(t *testing.T) {
	s, err := Parse("k8s.yaml", []byte("service: k8s\ntargets:\n  - namespace: prod\n    pod: api-*\n  - svm_ci: [CI1, CI2]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Targets[0].Fields["namespace"] != "prod" || s.Targets[0].Fields["pod"] != "api-*" || s.Targets[0].ID() != "namespace=prod,pod=api-*" {
		t.Fatalf("свободные поля: %+v", s.Targets[0])
	}
	if strings.Join(s.Targets[1].CIList, ",") != "CI1,CI2" || s.Targets[1].ID() != "CI1,CI2" {
		t.Fatalf("svm_ci списком: %+v", s.Targets[1])
	}

	// По проводу модулю — плоский объект, и обратно то же самое.
	data, err := json.Marshal(s.Targets)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"namespace":"prod"`) || !strings.Contains(string(data), `"ci_list":["CI1","CI2"]`) {
		t.Fatalf("JSON: %s", data)
	}
	var back []Target
	if err := json.Unmarshal(data, &back); err != nil || back[0].Fields["pod"] != "api-*" || len(back[1].CIList) != 2 {
		t.Fatalf("обратно: %+v, %v", back, err)
	}
}

func TestResolveTargetsByInventory(t *testing.T) {
	hosts, err := inventory.ParseFile("inv.yaml", []byte(`
groups:
  db: {max_connections: 200}
hosts:
  db1: {groups: [db], ip: 10.0.0.1, svm_ci: CI001}
  db2: {groups: [db, db_new], ip: 10.0.0.2, svm_ci: CI002}
  web1: {group: web}
`))
	if err != nil {
		t.Fatal(err)
	}
	limit := func(texts ...string) []inventory.Selector {
		sels, err := inventory.ParseSelectors(texts)
		if err != nil {
			t.Fatal(err)
		}
		return sels
	}
	parse := func(src string) *Scenario {
		s, err := Parse("s.yaml", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Группа разворачивается в хосты со своими полями; поля цели — поверх.
	s := parse("service: tune\ntargets: [{group: db, role: replica}, {namespace: prod}]\n")
	if !s.UsesInventory() {
		t.Fatal("цель group не выбирает из инвентаря")
	}
	if err := s.ResolveTargets(hosts, nil); err != nil {
		t.Fatal(err)
	}
	if len(s.Targets) != 3 || s.Targets[0].SVMCI != "CI001" || s.Targets[0].IP != "10.0.0.1" ||
		s.Targets[0].Fields["max_connections"] != "200" || s.Targets[0].Fields["role"] != "replica" ||
		s.Targets[1].Fields["groups"] != "db,db_new" || s.Targets[2].Fields["namespace"] != "prod" {
		t.Fatalf("цели: %+v", s.Targets)
	}

	// --limit сужает цели — и развёрнутые, и написанные в сценарии.
	s = parse("service: tune\ntargets: [{group: db}, {namespace: prod}]\n")
	if err := s.ResolveTargets(hosts, limit("group=db_new")); err != nil || len(s.Targets) != 1 || s.Targets[0].SVMCI != "CI002" {
		t.Fatalf("--limit group=db_new: %+v, %v", s.Targets, err)
	}
	s = parse("service: tune\ntargets: [{svm_ci: CI1}, {svm_ci: CI2}]\n")
	if err := s.ResolveTargets(nil, limit("svm_ci=CI2")); err != nil || len(s.Targets) != 1 || s.Targets[0].SVMCI != "CI2" {
		t.Fatalf("--limit без инвентаря: %+v, %v", s.Targets, err)
	}

	for src, want := range map[string]string{
		"service: tune\ntargets: [{group: cache}]\n":         "в инвентаре нет подходящих хостов",
		"service: tune\ntargets: [{group: \"db,web\"}]\n":    "одно имя или шаблон",
		"service: tune\nitems: [{a: 1}]\n":                   "отбирает цели, а у сценария их нет",
		"service: tune\ntargets: [{group: db}]\nitems: []\n": "не оставил ни одной цели",
	} {
		sels := limit("group=web")
		if strings.Contains(src, "cache") || strings.Contains(src, "db,web") {
			sels = nil
		}
		if err := parse(src).ResolveTargets(hosts, sels); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, ожидалось %q", src, err, want)
		}
	}
}
