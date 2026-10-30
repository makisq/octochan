package cmd

import (
	"fmt"
	"slices"
	"strings"

	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

// Порядок между заявками и что бывает при сбое — workspace/order.go.

// resolveAfter — заявки из --after полными идентификаторами. На
// отклонённую, упавшую или ту, что сама не выполнится, опереться нельзя:
// новая заявка не выполнилась бы никогда.
func resolveAfter(store *workspace.Requests, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	graph := workspace.NewGraph(all)
	var out []string
	for _, id := range ids {
		dep, err := store.Get(strings.TrimSpace(id))
		if err != nil {
			return nil, fmt.Errorf("--after: %w", err)
		}
		if err := checkRequestScope(currentRole, dep); err != nil {
			return nil, fmt.Errorf("--after: %w", err)
		}
		probe := &workspace.Request{After: []string{dep.ID}}
		if broken := graph.Readiness(probe).Broken; len(broken) > 0 {
			return nil, fmt.Errorf("--after %s: заявка %s — после неё ничего не выполнится", dep.ID, broken[0].Reason)
		}
		if !slices.Contains(out, dep.ID) {
			out = append(out, dep.ID)
		}
	}
	return out, nil
}

// checkOrder — заявку держат зависимости: выполнять её рано или уже
// бессмысленно. Проверяется до выполнения, и состояние заявки не меняется.
func checkOrder(store *workspace.Requests, request *workspace.Request) error {
	if len(request.After) == 0 {
		return nil
	}
	all, err := store.List()
	if err != nil {
		return err
	}
	graph := workspace.NewGraph(all)
	readiness := graph.Readiness(request)
	if len(readiness.Broken) > 0 {
		return fmt.Errorf("заявка %s не выполнится: %s. Она строилась в расчёте на них — оформите её заново "+
			"после новых заявок или отклоните", request.ID, joinBlockers(readiness.Broken))
	}
	if len(readiness.Waiting) > 0 {
		var states []string
		for _, id := range readiness.Waiting {
			states = append(states, fmt.Sprintf("%s (%s)", id, graph[id].State.Russian()))
		}
		return fmt.Errorf("заявка %s ожидает: зависит от %s — сначала ochan apply --request %s",
			request.ID, strings.Join(states, ", "), readiness.Waiting[0])
	}
	return nil
}

func joinBlockers(blockers []workspace.Blocker) string {
	out := make([]string, len(blockers))
	for i, b := range blockers {
		out[i] = b.String()
	}
	return strings.Join(out, "; ")
}

// markOutcome записывает итог выполнения. Успех — выполнена. Отказ
// исполнителя до первого обращения к системе — заявка остаётся
// согласованной: ничего не начиналось. Всё остальное — упала, и
// интерфейс говорит, что это значит для заявок вокруг неё.
func markOutcome(cmd *cobra.Command, store *workspace.Requests, request *workspace.Request, refs []string, runErr error) {
	if runErr != nil {
		noteRequestStep(request, "apply", "упала: "+runErr.Error())
	} else {
		noteRequestStep(request, "apply", "выполнена")
	}
	if runErr == nil {
		if err := store.MarkApplied(request, refs); err != nil {
			cmd.PrintErrf("внимание: не удалось отметить заявку выполненной: %v\n", err)
		}
		return
	}
	if scenario.IsNotStarted(runErr) && len(refs) == 0 {
		if err := store.Unrun(request); err != nil {
			cmd.PrintErrf("внимание: %v\n", err)
		}
		cmd.PrintErrf("Выполнение не начиналось: заявка %s остаётся согласованной.\n", request.ID)
		return
	}
	if err := store.MarkFailed(request, refs, runErr); err != nil {
		cmd.PrintErrf("внимание: не удалось отметить заявку упавшей: %v\n", err)
	}
	explainFailure(cmd, store, request)
}

// explainFailure — что сбой заявки значит для заявок до и после неё:
// «A остаётся, B пересобрать».
func explainFailure(cmd *cobra.Command, store *workspace.Requests, request *workspace.Request) {
	cmd.PrintErrf("Заявка %s упала.\n", request.ID)
	if len(request.AppliedRef) > 0 {
		cmd.PrintErrf("  До сбоя сделано: %s.\n", strings.Join(request.AppliedRef, ", "))
	} else {
		cmd.PrintErrln("  До сбоя ничего не записано.")
	}
	if len(request.After) > 0 {
		cmd.PrintErrf("  Заявки, после которых она шла, остаются выполненными — её сбой их не откатывает: %s.\n",
			strings.Join(request.After, ", "))
	}
	again := "ochan request " + request.Source
	if request.Service == "" && request.Script == nil {
		again = "ochan request " + request.Target + " " + request.Source
	}
	for _, id := range request.After {
		again += " --after " + id
	}
	cmd.PrintErrf("  Заявку не повторяют: оформите её заново (%s) и согласуйте.\n", again)
	if request.Service != "" && len(request.AppliedRef) > 0 {
		cmd.PrintErrf("  Вернуть сделанное до сбоя: ochan compensate %s.\n", request.ID)
	}
	all, err := store.List()
	if err != nil {
		return
	}
	if downstream := workspace.NewGraph(all).Downstream(request.ID); len(downstream) > 0 {
		ids := make([]string, len(downstream))
		for i, r := range downstream {
			ids[i] = r.ID
		}
		cmd.PrintErrf("  Не выполнятся, пока их не оформят заново после новой: %s.\n", strings.Join(ids, ", "))
	}
}

// warnDependents предупреждает, что откат заявки не откатывает
// выполненные после неё: они строились в расчёте на неё.
func warnDependents(cmd *cobra.Command, id string) {
	store, err := requestStore()
	if err != nil {
		return
	}
	all, err := store.List()
	if err != nil {
		return
	}
	var applied []string
	for _, r := range workspace.NewGraph(all).Dependents(id) {
		if r.State == workspace.RequestApplied {
			applied = append(applied, r.ID)
		}
	}
	if len(applied) > 0 {
		cmd.PrintErrf("внимание: после заявки %s выполнены заявки %s, построенные в расчёте на неё; "+
			"их этот откат не трогает\n", id, strings.Join(applied, ", "))
	}
}
