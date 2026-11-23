package cmd

import (
	"strings"
	"testing"

	"ochan/core/rbac"

	"github.com/spf13/cobra"
)

// Класс команды модуля — из аннотации class: read-команду выполняет llm,
// исполнительную — нет, и ochan modules --json называет классы.
func TestModuleClassesFromAnnotations(t *testing.T) {
	e := newScriptEnv(t)
	ran := ""
	module := &cobra.Command{Use: "pgstat", Annotations: map[string]string{
		moduleCommandAnnotation: "module", rbac.ClassAnnotation: "read"}}
	module.AddCommand(&cobra.Command{Use: "show", RunE: func(*cobra.Command, []string) error { ran = "show"; return nil }},
		&cobra.Command{Use: "reset", Annotations: map[string]string{rbac.ClassAnnotation: "execution"},
			RunE: func(*cobra.Command, []string) error { ran = "reset"; return nil }})
	raw := &cobra.Command{Use: "pgraw", Annotations: map[string]string{moduleCommandAnnotation: "module"},
		RunE: func(*cobra.Command, []string) error { ran = "raw"; return nil }}
	rootCmd.AddCommand(module, raw)
	t.Cleanup(func() {
		rootCmd.RemoveCommand(module, raw)
		rbac.SetClasses(nil)
	})
	if warnings := syncClasses(); len(warnings) > 0 {
		t.Fatalf("предупреждения: %v", warnings)
	}

	listing := e.must("modules", "--json")
	for _, want := range []string{`"command": "pgstat show"`, `"class": "read"`, `"command": "pgstat reset"`, `"class": "apply"`, `"command": "pgraw"`} {
		if !strings.Contains(listing, want) {
			t.Fatalf("modules --json без %s:\n%s", want, listing)
		}
	}

	e.must("set", "role", "llm")
	e.must("pgstat", "show")
	if ran != "show" {
		t.Fatalf("read-команда модуля не выполнилась у llm: %q", ran)
	}
	e.fails("нужно право apply", "pgstat", "reset")
	e.fails("нужно право pgraw или all", "pgraw")
}
