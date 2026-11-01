// Пакетная проверка текстов ℒ для корпуса своей модели — тот же разбор и те
// же проверки, что у ochan, без запуска ochan на каждый текст.
//
// На stdin — JSON-строки {"id": "…", "text": "…", "model": true}, на
// stdout — {"id": "…", "ok": true, "syntax": true, "codes": "…", "first": "…"}:
// ok — отказов нет, syntax — текст в языке (⊥syn нет). model — текст
// модели: строже, без комментариев и символов вне ASCII. answer — ответ,
// который строит логика ochan (fp.PlanAnswer): копия задачи и решения,
// выведенные из посылок; для текста не в языке — пусто.
//
//	go run ./llama/fpcheck < texts.jsonl > verdicts.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"octochan/arbiter"
	"octochan/fp"
)

type input struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Model bool   `json:"model"`
}

type verdict struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Syntax bool   `json:"syntax"`
	Codes  string `json:"codes,omitempty"`
	First  string `json:"first,omitempty"`
	Answer string `json:"answer,omitempty"`
}

func main() {
	opts := fp.Options{Lexicon: fp.BaseLexicon(), Builtins: arbiter.Builtins}
	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)
	for {
		var in input
		if err := dec.Decode(&in); err == io.EOF {
			return
		} else if err != nil {
			fmt.Fprintln(os.Stderr, "fpcheck:", err)
			os.Exit(1)
		}
		var r *fp.Result
		if in.Model {
			r = fp.CheckModelText(in.ID, in.Text, opts)
		} else {
			r = fp.CheckText(in.ID, in.Text, opts)
		}
		v := verdict{ID: in.ID, OK: r.Errors() == 0, Syntax: len(r.Syntax()) == 0}
		if v.Syntax && !in.Model {
			v.Answer = fp.PlanAnswer(in.Text, r.Program).Text()
		}
		counts := map[string]int{}
		for _, d := range r.Diagnostics {
			if !d.IsError() {
				continue
			}
			counts[string(d.Code)]++
			if v.First == "" {
				v.First = d.String()
			}
		}
		var codes []string
		for code, n := range counts {
			codes = append(codes, fmt.Sprintf("%s %d", code, n))
		}
		sort.Strings(codes)
		v.Codes = strings.Join(codes, ", ")
		if err := enc.Encode(v); err != nil {
			fmt.Fprintln(os.Stderr, "fpcheck:", err)
			os.Exit(1)
		}
	}
}
