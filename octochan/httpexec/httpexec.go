// Package httpexec — исполнитель сценариев ядра: запрос к API узла.
//
// Самое нужное из RLM, без RLM: отправить плейбук узлу по HTTP (POST и
// другие глаголы) и вернуть его ответ. Сценарий:
//
//	service: http
//	targets:
//	  - host: node_n
//	parameters:
//	  url: https://{host}/api/tune   # {host}, {ip}, {id} — из цели
//	  method: POST                   # POST по умолчанию; PUT, PATCH, DELETE, GET
//	  headers: {X-Team: ops}
//	  work_mem: 64MB                 # остальное — тело запроса, JSON
//	  string_i: ?                    # общая строка для f @ (…)
//
// Без url — адрес из config.yaml: executors.http.api_url (или
// defaults.api_url) и path сценария. Тело — body, если он задан, иначе все
// параметры, кроме служебных. Токен — ochan auth --executor http: он уходит
// в заголовке Authorization со схемой executors.http.auth_scheme (Bearer).
//
// Ответ узла — не в выводе и не в журнале: тела ответов идут в
// Result.Responses, и f @ (…) языка ищет в них общие строки — string_i из
// ответа на сценарий b попадает в плейбук c.
package httpexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"ochan/config"
	"ochan/scenario"
)

// Service — сервис сценариев, который выполняет этот исполнитель.
const Service = "http"

// reserved — параметры запроса, а не тела.
var reserved = map[string]bool{"url": true, "path": true, "method": true, "headers": true, "body": true}

// methods — глаголы, которые исполнитель отправляет.
var methods = map[string]bool{http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true,
	http.MethodDelete: true, http.MethodGet: true}

// maxBody — предел ответа узла.
const maxBody = 4 << 20

// Supply — токен и настройки исполнителя на момент вызова: заданные в
// оболочке через auth и config.yaml действуют сразу.
type Supply func() (token string, settings map[string]string)

// Runner — исполнитель http.
type Runner struct {
	supply Supply
	client *http.Client
}

// New — исполнитель с токеном и настройками от supply.
func New(supply Supply) *Runner {
	return &Runner{supply: supply}
}

// Kind — как исполнитель называется пользователю.
func (*Runner) Kind() string { return "HTTP" }

// request — один запрос плана.
type request struct {
	target  string
	method  string
	url     string
	headers map[string]string
	body    []byte
}

// requests строит запросы сценария: по одному на цель.
func (r *Runner) requests(s *scenario.Scenario) ([]request, error) {
	_, settings := r.settings()
	params := s.Parameters
	method := strings.ToUpper(text(params["method"]))
	if method == "" {
		method = http.MethodPost
	}
	if !methods[method] {
		return nil, fmt.Errorf("http: метод %s не поддерживается — POST, PUT, PATCH, DELETE, GET", method)
	}
	pattern := text(params["url"])
	if pattern == "" {
		base := settings["api_url"]
		if base == "" {
			return nil, errors.New("http: нет адреса — parameters.url в сценарии или executors.http.api_url в config.yaml")
		}
		pattern = strings.TrimRight(base, "/")
		if path := text(params["path"]); path != "" {
			pattern += "/" + strings.TrimLeft(path, "/")
		}
	}
	headers := map[string]string{}
	if h, ok := params["headers"].(map[string]any); ok {
		for k, v := range h {
			headers[k] = text(v)
		}
	}
	var payload any
	if b, ok := params["body"]; ok {
		payload = b
	} else {
		fields := map[string]any{}
		for k, v := range params {
			if !reserved[k] {
				fields[k] = v
			}
		}
		payload = fields
	}
	var body []byte
	if method != http.MethodGet {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("http: тело запроса: %w", err)
		}
		body = data
	}

	targets := s.Targets
	if len(targets) == 0 {
		targets = []scenario.Target{{}}
	}
	var out []request
	for _, t := range targets {
		host := t.Fields["host"]
		if host == "" {
			host = t.IP
		}
		replacer := strings.NewReplacer("{host}", host, "{ip}", t.IP, "{id}", t.ID())
		address := replacer.Replace(pattern)
		u, err := url.Parse(address)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("http: адрес %q: нужен http:// или https:// с именем узла", address)
		}
		id := t.ID()
		if id == "" {
			id = u.Host
		}
		out = append(out, request{target: id, method: method, url: address, headers: headers, body: body})
	}
	return out, nil
}

// Plan — какие запросы уйдут. В сеть не ходит; значения тела не
// показывает (в нём могут быть секреты) — только ключи.
func (r *Runner) Plan(_ context.Context, s *scenario.Scenario) (*config.Plan, error) {
	reqs, err := r.requests(s)
	if err != nil {
		return nil, err
	}
	plan := &config.Plan{Command: "apply"}
	for _, q := range reqs {
		plan.Add(config.Change{Kind: config.ChangeTask, Target: q.target,
			Name: q.method + " " + shown(q.url), Detail: bodyKeys(q.body)})
	}
	return plan, nil
}

// Apply отправляет ровно запросы согласованного плана.
func (r *Runner) Apply(ctx context.Context, s *scenario.Scenario, plan *config.Plan) (*scenario.Result, error) {
	reqs, err := r.requests(s)
	if err != nil {
		return nil, scenario.NotStarted(err)
	}
	if plan != nil && plan.Len() != len(reqs) {
		return nil, scenario.NotStarted(fmt.Errorf("http: запросов %d, а в согласованном плане %d", len(reqs), plan.Len()))
	}
	ctx, responses := scenario.WithResponses(ctx)
	result := &scenario.Result{}
	defer func() { result.Responses = responses.Bodies() }()

	token, settings := r.settings()
	client := r.client
	if client == nil {
		timeout := 30 * time.Second
		if d, err := time.ParseDuration(settings["http_timeout"]); err == nil && d > 0 {
			timeout = d
		}
		client = &http.Client{Timeout: timeout}
	}
	scheme := settings["auth_scheme"]
	if scheme == "" {
		scheme = "Bearer"
	}

	for i, q := range reqs {
		req, err := http.NewRequestWithContext(ctx, q.method, q.url, bytes.NewReader(q.body))
		if err != nil {
			return result, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "ochan")
		if q.body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", scheme+" "+token)
		}
		for k, v := range q.headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			if i == 0 {
				// Узел не ответил на первый же запрос — ничего не сделано.
				return result, scenario.NotStarted(fmt.Errorf("http: %s %s: %w", q.method, shown(q.url), err))
			}
			return result, fmt.Errorf("http: %s %s: %w", q.method, shown(q.url), err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		scenario.RecordResponse(ctx, data)
		if readErr != nil {
			return result, fmt.Errorf("http: ответ %s: %w", shown(q.url), readErr)
		}
		ref := fmt.Sprintf("%s %s %s → %d", q.target, q.method, shown(q.url), resp.StatusCode)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return result, fmt.Errorf("http: %s %s ответил %s: %s", q.method, shown(q.url), resp.Status, brief(data))
		}
		result.Refs = append(result.Refs, ref)
	}
	result.Summary = fmt.Sprintf("запросов: %d", len(result.Refs))
	return result, nil
}

func (r *Runner) settings() (string, map[string]string) {
	if r.supply == nil {
		return "", map[string]string{}
	}
	token, settings := r.supply()
	if settings == nil {
		settings = map[string]string{}
	}
	return token, settings
}

func text(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// shown — адрес без логина и пароля.
func shown(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return address
	}
	u.User = nil
	return u.String()
}

// bodyKeys — ключи тела для плана, без значений.
func bodyKeys(body []byte) string {
	var fields map[string]any
	if json.Unmarshal(body, &fields) != nil || len(fields) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "тело: " + strings.Join(keys, ", ")
}

// brief — начало ответа для сообщения об ошибке.
func brief(data []byte) string {
	s := strings.TrimSpace(string(data))
	if len([]rune(s)) > 300 {
		s = string([]rune(s)[:300]) + "…"
	}
	return s
}
