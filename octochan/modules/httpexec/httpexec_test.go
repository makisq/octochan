package httpexec

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ochan/core/scenario"
)

// Плейбук уходит узлу POST-запросом: тело — параметры без служебных, токен
// — в Authorization, ответ — в Result.Responses, не в итоге.
func TestApplyPostsPlaybook(t *testing.T) {
	var got map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &got)
		if got["string_i"] != "k-42" {
			http.Error(w, `{"detail": "нет string_i"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok": true, "next": {"token": "t-9"}}`))
	}))
	defer srv.Close()

	r := New(func() (string, map[string]string) { return "s3cr3t", map[string]string{"auth_scheme": "Token"} })
	yaml := "service: http\ntargets:\n  - ip: 127.0.0.1\nparameters:\n  url: " + srv.URL + "/api/{ip}/tune\n  work_mem: 64MB\n  string_i: k-42\n"
	s, err := scenario.Parse("c.yaml", []byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Plan(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Len() != 1 || !strings.HasPrefix(plan.Changes[0].Name, "POST ") ||
		plan.Changes[0].Detail != "тело: string_i, work_mem" {
		t.Fatalf("план: %+v", plan.Changes)
	}
	result, err := r.Apply(context.Background(), s, plan)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Token s3cr3t" || path != "/api/127.0.0.1/tune" || got["work_mem"] != "64MB" || got["url"] != nil {
		t.Fatalf("запрос: %q %q %v", auth, path, got)
	}
	if len(result.Responses) != 1 || !strings.Contains(result.Responses[0], "t-9") ||
		strings.Contains(result.Summary+strings.Join(result.Refs, " "), "t-9") {
		t.Fatalf("итог: %+v", result)
	}

	// Плохой ответ — ошибка с телом, а тело — тоже в ответах.
	s.Parameters["string_i"] = "?"
	result, err = r.Apply(context.Background(), s, plan)
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "нет string_i") {
		t.Fatalf("ошибка: %v", err)
	}
	if len(result.Responses) != 1 {
		t.Fatalf("ответ на ошибку не записан: %+v", result)
	}
}

func TestRequestsValidation(t *testing.T) {
	r := New(func() (string, map[string]string) {
		return "", map[string]string{"api_url": "https://node.example/api"}
	})
	s := &scenario.Scenario{Service: Service, Parameters: map[string]any{"path": "/tune", "method": "put",
		"body": map[string]any{"a": 1}}}
	reqs, err := r.requests(s)
	if err != nil || len(reqs) != 1 || reqs[0].url != "https://node.example/api/tune" || reqs[0].method != "PUT" ||
		string(reqs[0].body) != `{"a":1}` {
		t.Fatalf("%+v %v", reqs, err)
	}
	s.Parameters = map[string]any{"method": "CONNECT", "url": "https://x"}
	if _, err := r.requests(s); err == nil {
		t.Fatal("CONNECT прошёл")
	}
	s.Parameters = map[string]any{"url": "file:///etc/passwd"}
	if _, err := r.requests(s); err == nil {
		t.Fatal("file:// прошёл")
	}
	if _, err := New(nil).requests(&scenario.Scenario{Service: Service, Parameters: map[string]any{}}); err == nil {
		t.Fatal("без адреса прошёл")
	}
}
