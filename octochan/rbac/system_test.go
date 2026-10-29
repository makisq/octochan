package rbac

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const systemText = `users:
  anna: [ops, llm]
  uid:1005: [dev]
  "*": [llm]
roles:
  ops:
    allow: [read, plan, request, approve, apply]
    deny: [serve]
  dev:
    allow: [read, plan]
  llm:
    allow: [read, plan, request, apply]
`

func TestSystemPolicy(t *testing.T) {
	p, err := ParseSystem("roles.yaml", []byte(systemText), nil)
	if err != nil {
		t.Fatal(err)
	}
	anna := Identity{UID: 1001, User: "anna"}
	if got := strings.Join(p.Allowed(anna), ","); got != "ops,llm" {
		t.Fatalf("anna: %s", got)
	}
	if got := strings.Join(p.Allowed(Identity{UID: 1005, User: "bob"}), ","); got != "dev" {
		t.Fatalf("uid:1005: %s", got)
	}
	if err := p.Permit(Identity{UID: 1007, User: "eve"}, "ops"); err == nil || !strings.Contains(err.Error(), "положены llm") {
		t.Fatalf("eve: %v", err)
	}
	if err := p.Permit(anna, Master); err == nil {
		t.Fatal("master положена anna")
	}
	if !p.Permits(anna, "approve") || p.Permits(anna, "serve") {
		t.Fatal("Permits")
	}

	// Хранилище может только сузить: all под потолком — потолок.
	role := p.Ceil(&Role{Name: "ops", Perms: []string{All}})
	for cmd, want := range map[string]bool{"apply": true, "approve": true, "serve": false, "role.create": false, "help": true} {
		if role.Allows(cmd) != want {
			t.Errorf("ops %s: %v", cmd, !want)
		}
	}
	narrowed := p.Ceil(&Role{Name: "ops", Perms: []string{"read"}})
	if narrowed.Allows("apply") {
		t.Error("хранилище сузило ops до read, а apply открыт")
	}
	// llm не выше встроенного потолка.
	if p.Ceil(&Role{Name: LLM, Perms: []string{All}}).Allows("apply") {
		t.Error("llm получила apply из файла")
	}
	// Роль, которой нет в политике, не может ничего, кроме общих команд.
	if r := p.Ceil(&Role{Name: "ghost", Perms: []string{All}}); r.Allows("diff") || !r.Allows("role") {
		t.Error("роль вне политики")
	}
	if p.Ceil(&Role{Name: Master}).Ceiling() != nil {
		t.Error("у master потолок")
	}

	for text, want := range map[string]string{
		"roles: {ops: {allow: [read]}}\n":                             "нет users",
		"users: {anna: [ops]}\n":                                      "роли ops нет в roles",
		"users: {anna: [master]}\nroles: {master: {allow: [read]}}\n": "master не ограничивается",
		"users: {anna: [ops]}\nroles: {ops: {allow: [raed]}}\n":       "неизвестное право",
		"users: {uid:x: [llm]}\nroles: {llm: {allow: [read]}}\n":      "после uid: — число",
	} {
		if _, err := ParseSystem("r.yaml", []byte(text), nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: ожидалось %q, получено %v", text, want, err)
		}
	}
}

func TestSystemFileCheck(t *testing.T) {
	// Нет файла — нет политики.
	if p, err := LoadSystem(filepath.Join(t.TempDir(), "none.yaml"), nil); p != nil || err != nil {
		t.Fatalf("без файла: %v %v", p, err)
	}
	// Файл в каталоге теста: владелец и каталоги не те — ему не верят.
	path := filepath.Join(t.TempDir(), "roles.yaml")
	if err := os.WriteFile(path, []byte(systemText), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSystem(path, nil); err == nil || !strings.Contains(err.Error(), "верить нельзя") {
		t.Fatalf("файл теста: %v", err)
	}
	// Системный файл, которым владеет root, проверку проходит.
	real, err := filepath.EvalSymlinks("/etc/passwd")
	if err != nil {
		t.Skip(err)
	}
	if err := checkSystemFile(real); err != nil {
		t.Skipf("/etc/passwd на этой машине не годится для проверки: %v", err)
	}
}
