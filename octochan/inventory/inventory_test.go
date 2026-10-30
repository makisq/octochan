package inventory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `
groups:
  db: {max_connections: 200, tier: data}
hosts:
  db1: {groups: [db], ip: 10.0.0.1, svm_ci: CI001}
  db2: {groups: [db, db_new], ip: 10.0.0.2, max_connections: 300}
  web1: {group: web, ip: 10.0.1.1}
`

func TestParseFileMergesGroupVars(t *testing.T) {
	hosts, err := ParseFile("inv.yaml", []byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 3 || hosts[0].Name != "db1" || hosts[2].Name != "web1" {
		t.Fatalf("хосты: %+v", hosts)
	}
	// Переменная хоста важнее переменной группы.
	if hosts[0].Vars["max_connections"] != 200 || hosts[1].Vars["max_connections"] != 300 ||
		hosts[1].Vars["tier"] != "data" || strings.Join(hosts[2].Groups, ",") != "web" {
		t.Fatalf("переменные: %+v", hosts)
	}
	for src, want := range map[string]string{
		"hosts:\n  db1: {host: x}\n":        "поле host — имя хоста",
		"hosts:\n  \"a b\": {}\n":           "без пробелов",
		"hosts:\n  db1: {groups: [1, 2]}\n": "не имя",
		"hosts:\n  db1: {}\nextra: true\n":  "extra",
		"hosts:\n  db1: {groups: {a: b}}\n": "список имён",
	} {
		if _, err := ParseFile("inv.yaml", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, ожидалось %q", src, err, want)
		}
	}
}

func TestSelectors(t *testing.T) {
	hosts, _ := ParseFile("inv.yaml", []byte(sample))
	names := func(sels ...string) string {
		parsed, err := ParseSelectors(sels)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range Filter(hosts, parsed) {
			out = append(out, h.Name)
		}
		return strings.Join(out, ",")
	}
	cases := map[string]string{
		"group=db":           "db1,db2",
		"group=db_new":       "db2",
		"db*":                "db1,db2",
		"host=web1":          "web1",
		"group=db,ip=*.0.1":  "db1",
		"max_connections=3*": "db2",
		"tier=data":          "db1,db2",
		"group=nothing":      "",
		// «!» — отрицание условия, «!=» — то же.
		"group=db,!group=db_new": "db1",
		"group=db,group!=db_new": "db1",
		"!db*":                   "web1",
		"!ip=*.0.1":              "db2,web1",
		"!group=nothing":         "db1,db2,web1",
	}
	for sel, want := range cases {
		if got := names(sel); got != want {
			t.Errorf("%s: %q, ожидалось %q", sel, got, want)
		}
	}
	// Несколько селекторов — подходит хотя бы под один.
	if got := names("group=db_new", "host=web1"); got != "db2,web1" {
		t.Errorf("или: %q", got)
	}
	for _, bad := range []string{"", "group=", "=db", "host=[", "a,=b", "!", "!group!=db", "!=db"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("%q разобран", bad)
		}
	}
}

func TestLoadSourcesAndDiscovery(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(a, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("hosts:\n  cache1: {ip: 10.0.2.1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hosts, err := Load(context.Background(), map[string]string{"main": a, "cmdb": "file://" + b})
	if err != nil || len(hosts) != 4 || hosts[0].Name != "cache1" || hosts[0].Source != "cmdb" {
		t.Fatalf("загрузка: %+v, %v", hosts, err)
	}
	if _, err := Load(context.Background(), map[string]string{"a": a, "b": a}); err == nil ||
		!strings.Contains(err.Error(), "хост db1 есть и в инвентаре a, и в b") {
		t.Fatalf("дубль: %v", err)
	}
	if _, err := Load(context.Background(), map[string]string{"x": "cmdb://srv"}); err == nil ||
		!strings.Contains(err.Error(), "схемы cmdb нет; есть: file") {
		t.Fatalf("неизвестная схема: %v", err)
	}

	// Схему регистрирует тот, кто умеет её читать.
	Register("static", func(string) (Source, error) {
		return staticSource{{Name: "agent1", Vars: map[string]any{"ip": "10.9.9.9"}}}, nil
	})
	if hosts, err := Load(context.Background(), map[string]string{"agents": "static://"}); err != nil || hosts[0].Name != "agent1" {
		t.Fatalf("своя схема: %+v, %v", hosts, err)
	}

	// Новый сервер — которого ещё не видели.
	seen, err := LoadSeen(filepath.Join(dir, "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(seen.New(hosts)) != len(hosts) {
		t.Fatal("пустой журнал: все хосты новые")
	}
	if err := seen.Mark(hosts, time.Now()); err != nil {
		t.Fatal(err)
	}
	again, _ := LoadSeen(filepath.Join(dir, "seen.json"))
	if len(again.New(hosts)) != 0 || again.IsNew(hosts[0]) {
		t.Fatal("отмеченный хост снова новый")
	}
	hosts[0].Source = "другой"
	if !again.IsNew(hosts[0]) {
		t.Fatal("тот же хост из другого источника — не новый")
	}
}

type staticSource []Host

func (s staticSource) Hosts(context.Context) ([]Host, error) { return s, nil }
