package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"octochan/audit"
	"octochan/i18n"
	"octochan/inventory"
	"octochan/rbac"
	"octochan/remote"
	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

// free — модель выполняет без человека, но только по политике.
//
// Только master — человек, у которого есть все права, — и только на
// primary контура HA-AI, где человека-оператора нет. master передаёт
// модели: роль, чьи права на выполнение она берёт (--role), политику —
// что выполнять без человека (--policy), — и срок (--for). Заявка модели
// (роль llm), которая укладывается в политику, согласуется ею и
// выполняется сразу, от имени этой роли и с её пределами; вне политики —
// обычная заявка, ждёт master. Срок, отзыв политики или её срок гасят free
// сами. Песочница, кольца, арбитр и сверка плана — как всегда.

const (
	freeFile = "free.json"
	// EnvAgent — агент, от которого работает модель: ochan-mcp --agent.
	// Задан — заявки модели проверяет только его free.
	EnvAgent = "OCTOCHAN_AGENT"
)

func freeStore() (*workspace.FreeGrants, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return workspace.NewFreeGrants(filepath.Join(fm.BaseDir(), freeFile)), nil
}

// aiContour — этот узел — primary контура HA-AI. Нет — почему.
func aiContour() (string, error) {
	path, err := haStatePath()
	if err != nil {
		return "", err
	}
	st, err := remote.LoadHAState(path)
	if err != nil {
		return "", i18n.Errorf("узел не в паре серверов (ochan ha init --ai-contour)")
	}
	switch {
	case st.Contour != remote.ContourAI:
		return "", i18n.Errorf("узел %s не в контуре HA-AI (ochan ha contour ai)", st.Node)
	case st.Role != remote.RolePrimary || os.Getenv(EnvHAStandby) == "1":
		return "", i18n.Errorf("узел %s — standby: free действует только на primary", st.Node)
	}
	return st.Node, nil
}

func auditFree(agent, role, what string, decision error) {
	journal, err := auditJournal()
	if err != nil {
		return
	}
	_ = journal.Append(audit.Record{Event: audit.EventFree, Role: role, Command: "agent " + agent,
		Args: []string{what}, Remote: os.Getenv(EnvRemoteAddr), User: workspace.CurrentUser()},
		func(audit.Recent) error { return decision })
}

// settleFree закрывает истёкшие выдачи и пишет об этом в журнал.
func settleFree(store *workspace.FreeGrants) {
	expired, err := store.Settle(time.Now())
	if err != nil {
		return
	}
	for _, g := range expired {
		auditFree(g.Agent, g.Role, fmt.Sprintf("free истёк в %s — isolation вернулось в natural", g.Until.Local().Format("15:04")), nil)
	}
}

// activeFree — действующие выдачи; агент из окружения сужает их до своей.
func activeFree() ([]workspace.FreeGrant, error) {
	store, err := freeStore()
	if err != nil {
		return nil, err
	}
	settleFree(store)
	grants, err := store.List()
	if err != nil {
		return nil, err
	}
	active := workspace.Active(grants, time.Now())
	if agent := os.Getenv(EnvAgent); agent != "" {
		var mine []workspace.FreeGrant
		for _, g := range active {
			if g.Agent == agent {
				mine = append(mine, g)
			}
		}
		return mine, nil
	}
	return active, nil
}

// grantFree выдаёт free агенту.
func grantFree(agent, role, window, policyName string) (*workspace.FreeGrant, error) {
	if currentRole == nil || currentRole.Name != rbac.Master {
		who := "без роли"
		if currentRole != nil {
			who = "роль " + currentRole.Name
		}
		return nil, i18n.Errorf("free включает только master (здесь %s): free делегирует права master, а не выдаёт новые", who)
	}
	node, err := aiContour()
	if err != nil {
		return nil, i18n.Errorf("отказ: free доступен только на узлах с HA-AI (контур без человека): %v", err)
	}
	if err := checkAgentName(agent); err != nil {
		return nil, err
	}
	if path, err := agentsPath(); err == nil {
		reg, err := loadAgents(path)
		if err != nil {
			return nil, err
		}
		if reg.Agents[agent] == nil {
			return nil, fmt.Errorf("агента %s нет в реестре: free выдают агенту, который сообщил о себе (ochan agent run)", agent)
		}
	}
	roles, _, err := loadRoles()
	if err != nil {
		return nil, err
	}
	r := roles.Find(role)
	switch {
	case role == "":
		return nil, fmt.Errorf("--role: чьи права на выполнение модель берёт")
	case r == nil:
		return nil, fmt.Errorf("роли %s нет", role)
	case strings.EqualFold(role, rbac.Master) || strings.EqualFold(role, rbac.LLM):
		return nil, fmt.Errorf("роль %s free не передаётся: master — человек, llm — сама модель", role)
	case !r.Allows("apply"):
		return nil, fmt.Errorf("у роли %s нет права apply: передавать модели нечего", role)
	}
	if policyName == "" {
		return nil, fmt.Errorf("--policy обязателен: без политики free был бы вседозволенностью")
	}
	policies, err := policyStore()
	if err != nil {
		return nil, err
	}
	p, err := policies.Get(policyName)
	if err != nil {
		return nil, err
	}
	if err := p.State(time.Now()); err != nil {
		return nil, err
	}
	d, err := rbac.ParseFor(window)
	if err != nil {
		return nil, fmt.Errorf("--for: %v", err)
	}
	now := time.Now()
	g := workspace.FreeGrant{Agent: agent, Role: role, Policy: p.Name, Window: window, Start: now,
		Until: now.Add(d), By: whoAmI()}
	if p.Expires.Before(g.Until) {
		// Политика кончится раньше — free кончится вместе с ней.
		g.Until = p.Expires
	}
	store, err := freeStore()
	if err != nil {
		return nil, err
	}
	settleFree(store)
	if err := store.Grant(g); err != nil {
		return nil, err
	}
	auditFree(agent, role, fmt.Sprintf("free включён на %s: роль %s, политика %s, до %s (%s)", node, role, p.Name,
		g.Until.Local().Format("2006-01-02 15:04"), g.By), nil)
	return &g, nil
}

func freeText(g *workspace.FreeGrant) string {
	return i18n.Sprintf("free (до %s, роль %s, политика %s)", g.Until.Local().Format("15:04"), g.Role, g.Policy)
}

// freeRequest — заявка модели на узле HA-AI под free: в политике —
// согласована ею и выполнена от имени роли выдачи; вне — ждёт master.
func freeRequest(cmd *cobra.Command, store *workspace.Requests, request *workspace.Request) {
	if currentRole == nil || currentRole.Name != rbac.LLM || request.Service == "" {
		return
	}
	if _, err := aiContour(); err != nil {
		return
	}
	grants, err := activeFree()
	if err != nil || len(grants) == 0 {
		return
	}
	if err := incidentOpenForFree(request); err != nil {
		cmd.Printf("Заявка %s ждёт человека: %v\n", request.ID, err)
		return
	}
	parsed, err := requestScenario(cmd.Context(), request)
	if err != nil {
		cmd.Printf("free: сценарий заявки не разобран: %v — ждёт master\n", err)
		return
	}
	var outside []string
	for i := range grants {
		g := &grants[i]
		reasons, role := freeVerdict(cmd, g, request, parsed)
		if len(reasons) > 0 {
			outside = append(outside, fmt.Sprintf("%s (%s): %s", g.Agent, g.Policy, strings.Join(reasons, "; ")))
			continue
		}
		request.Policy, request.Free = g.Policy, g.Agent
		who := fmt.Sprintf("free: агент %s, политика %s (включил %s) — без человека", g.Agent, g.Policy, g.By)
		if err := store.Decide(request, workspace.RequestApproved, who, "заявка модели в пределах политики"); err != nil {
			cmd.PrintErrf("free: %v\n", err)
			return
		}
		auditFree(g.Agent, g.Role, fmt.Sprintf("approve %s (free: политика %s, без человека)", request.ID, g.Policy), nil)
		cmd.Printf("Заявка %s в free агента %s (политика %s) — выполняется без человека, от имени роли %s\n",
			request.ID, g.Agent, g.Policy, g.Role)
		saved := currentRole
		currentRole = role
		err := applyApprovedRequest(cmd, request.ID)
		currentRole = saved
		if err != nil {
			auditFree(g.Agent, g.Role, fmt.Sprintf("apply %s (free) не удался", request.ID), err)
			cmd.Printf("free: выполнение не удалось: %v\n", err)
			return
		}
		auditFree(g.Agent, g.Role, fmt.Sprintf("apply %s (free: политика %s, без человека)", request.ID, g.Policy), nil)
		return
	}
	why := fmt.Errorf("вне free: %s", strings.Join(outside, " | "))
	auditFree(grants[0].Agent, rbac.LLM, fmt.Sprintf("request %s — ждёт master", request.ID), why)
	cmd.Printf("Заявка %s %s — ждёт master\n", request.ID, why)
}

// freeVerdict — почему заявка не укладывается в free; пусто — укладывается.
// Те же условия, что у ochan watch: сервис, цели, параметры, план без
// перерыва обслуживания, право и предел роли на apply.
func freeVerdict(cmd *cobra.Command, g *workspace.FreeGrant, request *workspace.Request,
	parsed *scenario.Scenario) ([]string, *rbac.Role) {
	policies, err := policyStore()
	if err != nil {
		return []string{err.Error()}, nil
	}
	p, err := policies.Get(g.Policy)
	if err != nil {
		return []string{err.Error()}, nil
	}
	if err := p.State(time.Now()); err != nil {
		return []string{err.Error()}, nil
	}
	var out []string
	if parsed.Service != p.Service {
		out = append(out, fmt.Sprintf("сервис %s, а политика — для %s", parsed.Service, p.Service))
	}
	sels, err := inventory.ParseSelectors(p.Targets)
	if len(parsed.Targets) == 0 {
		out = append(out, "у заявки нет целей — политика покрывает только цели")
	}
	for i := range parsed.Targets {
		t := &parsed.Targets[i]
		if err != nil || !inventory.MatchAny(sels, t.Labels()) {
			out = append(out, fmt.Sprintf("цель %s не подходит под targets (%s)", targetName(t, i), strings.Join(p.Targets, ", ")))
		}
	}
	out = append(out, paramsOutside(p, parsed)...)
	if request.Plan != nil && request.Plan.Destructive {
		out = append(out, "план прерывает обслуживание — такое только через человека")
	}
	roles, _, err := loadRoles()
	if err != nil {
		return append(out, err.Error()), nil
	}
	role := roles.Find(g.Role)
	switch {
	case role == nil:
		return append(out, fmt.Sprintf("роли %s больше нет", g.Role)), nil
	case len(out) > 0:
		return out, role
	}
	if err := admit(cmd, role, "apply", []string{"--request", request.ID}); err != nil {
		out = append(out, err.Error())
	}
	return out, role
}

var (
	freeRole   string
	freeFor    string
	freePolicy string
	freeRevoke bool
)

var agentsFreeCmd = &cobra.Command{
	Use:   "free <агент> --role <роль> --for <срок> --policy <политика>",
	Short: "Включить free агенту: модель выполняет без человека по политике",
	Long: "Только master и только на primary контура HA-AI (ochan ha contour ai).\n" +
		"Заявка модели (роль llm), которая укладывается в политику, согласуется ею и\n" +
		"выполняется сразу — от имени --role, с её правами и пределами. Вне\n" +
		"политики — обычная заявка: ждёт master. free гаснет по --for, с отзывом\n" +
		"или сроком политики и с --revoke. Песочница, кольца, арбитр и сверка плана\n" +
		"действуют как всегда: free меняет только то, кто сказал «да».",
	Args: cobra.ExactArgs(1),
	Example: "ochan agents free db7 --role deployer --for 4h --policy new-servers\n" +
		"ochan agents free db7 --revoke",
	RunE: func(cmd *cobra.Command, args []string) error {
		if freeRevoke {
			if currentRole == nil || currentRole.Name != rbac.Master {
				return refuse(cmd, i18n.Errorf("free отзывает master"))
			}
			store, err := freeStore()
			if err != nil {
				return err
			}
			g, err := store.Revoke(args[0], "отозван ("+whoAmI()+")", time.Now())
			if err != nil {
				return err
			}
			auditFree(g.Agent, g.Role, "free отозван — isolation вернулось в natural ("+whoAmI()+")", nil)
			cmd.Printf("free агента %s отозван: isolation natural\n", g.Agent)
			return nil
		}
		g, err := grantFree(args[0], freeRole, freeFor, freePolicy)
		if err != nil {
			return refuse(cmd, err)
		}
		cmd.Printf("Агент %s: isolation %s\n", g.Agent, freeText(g))
		cmd.Printf("Отозвать: ochan agents free %s --revoke\n", g.Agent)
		return nil
	},
}

// freeLine — free агента для ochan agents.
func freeLine(agent string, grants []workspace.FreeGrant) string {
	for i := range grants {
		if grants[i].Agent == agent && grants[i].Open(time.Now()) {
			return freeText(&grants[i])
		}
	}
	return ""
}

// freeEligible — заявку модели на узле HA-AI стоит проверить на free.
func freeEligible(request *workspace.Request) bool {
	if currentRole == nil || currentRole.Name != rbac.LLM || request.Service == "" {
		return false
	}
	if _, err := aiContour(); err != nil {
		return false
	}
	grants, err := activeFree()
	return err == nil && len(grants) > 0
}

// freeCompensation — обратное действие заявки, которую выполнил free: пока
// он действует, модель возвращает своё сама, как и делала, — без человека
// и от имени той же роли. Чужое обратное действие ждёт человека.
func freeCompensation(cmd *cobra.Command, store *workspace.Requests, original, request *workspace.Request) bool {
	if original.Free == "" || currentRole == nil || currentRole.Name != rbac.LLM {
		return false
	}
	if _, err := aiContour(); err != nil {
		return false
	}
	grants, err := activeFree()
	if err != nil {
		return false
	}
	for i := range grants {
		g := &grants[i]
		if g.Agent != original.Free {
			continue
		}
		roles, _, err := loadRoles()
		if err != nil {
			return false
		}
		role := roles.Find(g.Role)
		if role == nil {
			return false
		}
		if err := admit(cmd, role, "apply", []string{"--request", request.ID}); err != nil {
			cmd.Printf("free: %v — ждёт master\n", err)
			return false
		}
		request.Policy, request.Free = original.Policy, g.Agent
		who := fmt.Sprintf("free: агент %s — обратное действие своей заявки %s, без человека", g.Agent, original.ID)
		if err := store.Decide(request, workspace.RequestApproved, who, "обратное действие выполненного по free"); err != nil {
			cmd.PrintErrf("free: %v\n", err)
			return false
		}
		saved := currentRole
		currentRole = role
		err = applyApprovedRequest(cmd, request.ID)
		currentRole = saved
		auditFree(g.Agent, g.Role, fmt.Sprintf("compensate %s → %s (free, без человека)", original.ID, request.ID), err)
		if err != nil {
			cmd.Printf("free: обратное действие не выполнено: %v\n", err)
		}
		return true
	}
	return false
}
