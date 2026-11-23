package cmd

import (
	"testing"

	"ochan/lang"

	"github.com/spf13/cobra"
)

// approve @ (…) и модуль.команда @ (…) — инструкция языка, а не аргументы
// команды; общие строки команды модуля — из её аннотации shared.
func TestAtInShell(t *testing.T) {
	node := &cobra.Command{Use: "node", Annotations: map[string]string{moduleCommandAnnotation: "module"}}
	call := &cobra.Command{Use: "call", Annotations: map[string]string{lang.SharedAnnotation: "node_tune:string_i, token=session.token"},
		RunE: func(*cobra.Command, []string) error { return nil }}
	node.AddCommand(call)
	rootCmd.AddCommand(node)
	t.Cleanup(func() { rootCmd.RemoveCommand(node) })

	for line, want := range map[string]bool{
		`approve @ ("a" | "b")`:        true,
		`approve@("a")`:                true,
		`node.call @ ("x.yaml" | "y")`: true,
		`approve 1cc5ae2d97`:           false,
		`node call x.yaml`:             false,
		`r = approve @ ("a" | "b")`:    true,
		`print(approve @ ("a" | "b"))`: true,
	} {
		if got := shellStatement(line); got != want {
			t.Errorf("%s: инструкция — %v, ожидалось %v", line, got, want)
		}
	}

	keys := scriptCommands{}.Shared([]string{"node", "call"})
	if len(keys) != 2 || keys[0] != (lang.SharedKey{Service: "node_tune", Param: "string_i", From: "string_i"}) ||
		keys[1] != (lang.SharedKey{Param: "token", From: "session.token"}) {
		t.Fatalf("общие строки команды: %+v", keys)
	}
	if keys := (scriptCommands{}).Shared([]string{"diff"}); len(keys) != 0 {
		t.Fatalf("у команды ядра общие строки: %+v", keys)
	}
}
