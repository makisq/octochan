package module

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"ochan/config"
	"ochan/rlm"
	"ochan/scenario"
)

const pangolinTableID = "pangolinunique"

// PangolinRestartItem — элемент инвентаря для перезапуска.
type PangolinRestartItem struct {
	TableID               string `json:"table_id"`
	InvsvmCISvm           string `json:"invsvm_ci_svm,omitempty"`
	InvsvmIP              string `json:"invsvm_ip,omitempty"`
	InvsvmAliaces         string `json:"invsvm_aliaces,omitempty"`
	InvinstCIInst         string `json:"invinst_ci_inst,omitempty"`
	InvinstInstName       string `json:"invinst_inst_name,omitempty"`
	InvirCIIr             string `json:"invir_ci_ir,omitempty"`
	InvirIrName           string `json:"invir_ir_name,omitempty"`
	InvstendEnvironment   string `json:"invstend_environment,omitempty"`
	InvstendCIStend       string `json:"invstend_ci_stend,omitempty"`
	InvstendStendName     string `json:"invstend_stend_name,omitempty"`
	InvclusterCICluster   string `json:"invcluster_ci_cluster,omitempty"`
	InvclusterClusterName string `json:"invcluster_cluster_name,omitempty"`
	InvirpgseCIIr         string `json:"invirpgse_ci_ir,omitempty"`
	InvirpgseIrName       string `json:"invirpgse_ir_name,omitempty"`
}

// PangolinRestartParams — параметры перезапуска.
type PangolinRestartParams struct {
	Restart         string `json:"restart"`
	SkipSMConflicts bool   `json:"skip_sm_conflicts"`
	Confirm         bool   `json:"confirm"`
}

// PangolinRestartPayload — тело запроса на перезапуск.
type PangolinRestartPayload struct {
	Service  string                `json:"service"`
	StartAt  string                `json:"start_at"`
	Datetime string                `json:"datetime"`
	Params   PangolinRestartParams `json:"params"`
	Items    []PangolinRestartItem `json:"items"`
}

// PangolinRestartModule перезапускает экземпляры pangolin.
type PangolinRestartModule struct {
	data   *scenario.Scenario
	params Params
}

// NewPangolinRestartModule создаёт модуль сервиса pangolin_restart.
func NewPangolinRestartModule(data *scenario.Scenario) (rlm.ScenarioModule, error) {
	return &PangolinRestartModule{data: data, params: Params(data.Parameters)}, nil
}

// identityFields — поля, по которым RLM опознаёт экземпляр.
var identityFields = []string{
	"invsvm_ci_svm",
	"invsvm_ip",
	"invsvm_aliaces",
	"invinst_ci_inst",
	"invir_ci_ir",
}

func (m *PangolinRestartModule) Validate() error {
	hasItems := len(m.data.Items) > 0
	hasTargets := len(m.data.Targets) > 0

	switch {
	case !hasItems && !hasTargets:
		return fmt.Errorf("не указаны items или targets для перезапуска")
	case hasItems && hasTargets:
		// Прежняя версия формировала запросы и по items, и по targets,
		// то есть перезапускала одни и те же серверы дважды.
		return fmt.Errorf("укажите либо items, либо targets, но не оба сразу: " +
			"иначе перезапуск отправляется дважды")
	}

	for i, item := range m.data.Items {
		if !config.Any(identityFields, func(field string) bool {
			return itemString(item, field) != ""
		}) {
			return fmt.Errorf("item #%d должен содержать хотя бы один из параметров: %v", i+1, identityFields)
		}
	}

	for i, target := range m.data.Targets {
		if target.SVMCI == "" && target.IP == "" && len(target.CIList) == 0 {
			return fmt.Errorf("target #%d должен содержать svm_ci или ip", i+1)
		}
	}

	if err := m.params.RequirePresent("restart", "skip_sm_conflicts", "confirm"); err != nil {
		return err
	}
	if !m.params.Bool("confirm", false) {
		return fmt.Errorf("перезапуск не подтверждён: установите confirm: true")
	}

	return nil
}

func (m *PangolinRestartModule) GenerateRequests(_ context.Context, _ rlm.ScenarioAPI) ([]*rlm.APIRequest, error) {
	params := PangolinRestartParams{
		Restart:         m.params.String("restart", ""),
		SkipSMConflicts: m.params.Bool("skip_sm_conflicts", false),
		Confirm:         m.params.Bool("confirm", false),
	}

	var requests []*rlm.APIRequest

	if len(m.data.Items) > 0 {
		req, err := m.buildRequest(params, m.itemsFromScenario())
		if err != nil {
			return nil, err
		}
		requests = append(requests, req)
	}

	for _, target := range m.data.Targets {
		req, err := m.buildRequest(params, itemsFromTarget(target))
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", target.SVMCI, err)
		}
		requests = append(requests, req)
	}

	log.Printf("pangolin_restart: подготовлено запросов — %d", len(requests))
	return requests, nil
}

func (m *PangolinRestartModule) itemsFromScenario() []PangolinRestartItem {
	items := make([]PangolinRestartItem, 0, len(m.data.Items))
	for _, raw := range m.data.Items {
		tableID := itemString(raw, "table_id")
		if tableID == "" {
			tableID = pangolinTableID
		}

		items = append(items, PangolinRestartItem{
			TableID:               tableID,
			InvsvmCISvm:           itemString(raw, "invsvm_ci_svm"),
			InvsvmIP:              itemString(raw, "invsvm_ip"),
			InvsvmAliaces:         itemString(raw, "invsvm_aliaces"),
			InvinstCIInst:         itemString(raw, "invinst_ci_inst"),
			InvinstInstName:       itemString(raw, "invinst_inst_name"),
			InvirCIIr:             itemString(raw, "invir_ci_ir"),
			InvirIrName:           itemString(raw, "invir_ir_name"),
			InvstendEnvironment:   itemString(raw, "invstend_environment"),
			InvstendCIStend:       itemString(raw, "invstend_ci_stend"),
			InvstendStendName:     itemString(raw, "invstend_stend_name"),
			InvclusterCICluster:   itemString(raw, "invcluster_ci_cluster"),
			InvclusterClusterName: itemString(raw, "invcluster_cluster_name"),
			InvirpgseCIIr:         itemString(raw, "invirpgse_ci_ir"),
			InvirpgseIrName:       itemString(raw, "invirpgse_ir_name"),
		})
	}
	return items
}

func itemsFromTarget(target scenario.Target) []PangolinRestartItem {
	cis := target.GetCIs()
	if len(cis) == 0 {
		return []PangolinRestartItem{{TableID: pangolinTableID, InvsvmIP: target.IP}}
	}

	items := make([]PangolinRestartItem, 0, len(cis))
	for _, ci := range cis {
		items = append(items, PangolinRestartItem{
			TableID:     pangolinTableID,
			InvsvmCISvm: ci,
			InvsvmIP:    target.IP,
		})
	}
	return items
}

func (m *PangolinRestartModule) buildRequest(params PangolinRestartParams, items []PangolinRestartItem) (*rlm.APIRequest, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("пустой список items")
	}

	payload := PangolinRestartPayload{
		Service:  "pangolin_restart",
		StartAt:  "now",
		Datetime: time.Now().Format(time.RFC3339),
		Params:   params,
		Items:    items,
	}

	body, err := structToMap(payload)
	if err != nil {
		return nil, err
	}

	return &rlm.APIRequest{Method: "POST", Body: body}, nil
}

// structToMap переводит типизированный payload в map, как того требует
// rlm.APIRequest. Раньше эта пара Marshal/Unmarshal была скопирована
// в двух модулях, и рядом с ней стоял ещё один MarshalIndent, печатавший
// в лог весь запрос вместе с CI и адресами серверов.
func structToMap(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации запроса: %w", err)
	}

	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("ошибка преобразования запроса: %w", err)
	}
	return out, nil
}
