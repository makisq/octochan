package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Ядро не знает исполнителей: RLM и его сервисы приходят модулем
// (plugins/rlm). Импорт rlm или module в ядро вернул бы RLM обратно.
func TestCoreDoesNotImportRLM(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("нет go, чтобы перечислить зависимости")
	}
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if pkg == "octochan/rlm" || pkg == "octochan/module" {
			t.Errorf("ядро зависит от %s: RLM — модуль plugins/rlm, а не часть ядра", pkg)
		}
	}
}
