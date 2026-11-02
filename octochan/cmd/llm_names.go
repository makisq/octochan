package cmd

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"ochan/fp"
	"ochan/lang"

	"github.com/spf13/cobra"
)

// Имена, которые своя модель может писать (fp.Names): имена задачи и
// объекты, которые знает ochan, — хосты инвентаря, агенты, заявки и
// инциденты. Открытым именем (please_restart) модель говорила бы мимо
// языка, поэтому в её тексте имя проверяется по этому словарю.

// knownObjects — объекты ochan именами языка: db7, request_3f2a1c9d0e,
// incident_88fb030508. Источник, который не читается, пропускается:
// словарь сужает, а не открывает.
func knownObjects() func(string) bool {
	set := map[string]bool{}
	add := func(name, prefix string) {
		if name != "" {
			set[formalName(name, prefix)] = true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if hosts, err := loadInventory(ctx); err == nil {
		for _, h := range hosts {
			add(h.Name, "host")
		}
	}
	if path, err := agentsPath(); err == nil {
		if reg, err := loadAgents(path); err == nil {
			for name := range reg.Agents {
				add(name, "agent")
			}
		}
	}
	if store, err := requestStore(); err == nil {
		if all, err := store.List(); err == nil {
			for _, r := range all {
				add("request_"+r.ID, "request")
			}
		}
	}
	if store, err := incidentStore(); err == nil {
		if all, err := store.List(); err == nil {
			for _, i := range all {
				add("incident_"+i.ID, "incident")
			}
		}
	}
	return func(name string) bool { return set[name] || set[formalName(name, "host")] }
}

// modelNames — имена, которые своя модель может писать, отвечая на task.
func modelNames(task string) *fp.Names {
	n := fp.NamesOf(task)
	n.Known = knownObjects()
	return n
}

// foreignNames — имена в тексте своей модели, которых она знать не может.
func foreignNames(r *fp.Result, text, task string, opts fp.Options) []fp.Diagnostic {
	return r.ForeignNames(text, modelNames(task), opts.MaxMeanings)
}

// checkModelScript — сценарий и отчёт своей модели: в сценарии нет
// комментариев и строк не из задачи и объектов ochan, в отчёте — имён не
// оттуда же. Внешней модели (protocol) это не касается: строки ей нужны.
func checkModelScript(cmd *cobra.Command, in *scriptInput) error {
	if !modelEmbedded() {
		return nil
	}
	opts, err := fpOptions(cmd, false)
	if err != nil {
		return err
	}
	task := ""
	if in.prompt != nil {
		task = in.prompt.Content
	}
	var lines []string
	for _, e := range modelScriptErrors(in.script.Name, in.script.Content, task, opts) {
		lines = append(lines, "  "+e.Error())
	}
	if in.report != nil {
		r := fp.CheckModelText(in.report.Name, in.report.Content, opts)
		for _, d := range append(r.Syntax(), foreignNames(r, in.report.Content, task, opts)...) {
			lines = append(lines, "  "+in.report.Name+":"+d.String())
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("своя модель (llm.mode: embedded) пишет только на ℒ — в сценарии и отчёте нет естественного языка:\n%s",
		strings.Join(lines, "\n"))
}

// modelScriptErrors — что мешает принять сценарий своей модели:
// комментарии и строки, в которых не имя задачи или объекта ochan, не путь
// из binds_to задачи и не значение с единицей.
func modelScriptErrors(file, script, task string, opts fp.Options) []error {
	names := modelNames(task)
	lex := opts.Lexicon
	known := func(text string) bool {
		if _, unit, ok := fp.ParseNumber(text); ok {
			return unit == "" || unit == "%" || lex == nil && fp.BaseLexicon().IsUnit(unit) || lex != nil && lex.IsUnit(unit)
		}
		if strings.ContainsAny(text, " \t") {
			return false
		}
		if names.Path(text) {
			return true
		}
		for _, candidate := range []string{text, filepath.Base(text), path.Base(text)} {
			if names.Admits(candidate, lex, opts.MaxMeanings) || names.Admits(formalName(candidate, "name"), lex, opts.MaxMeanings) {
				return true
			}
		}
		return false
	}
	return lang.ModelText(file, script, known)
}

// modelEmbedded — пишет своя модель: роль llm в llm.mode: embedded.
func modelEmbedded() bool {
	if !senderIsModel() {
		return false
	}
	mode, err := llmMode()
	return err == nil && mode == llmEmbedded
}
