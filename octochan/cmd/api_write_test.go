package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Запрос, который меняет внешнюю систему, идёт заявкой: план его не шлёт,
// согласованная заявка шлёт ровно согласованное тело, а тело, которое
// разошлось с согласованным, заявку останавливает до отправки.
func TestScriptWriteRequestThroughApproval(t *testing.T) {
	var mu sync.Mutex
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}

	e := newScriptEnv(t)
	e.write(e.path("disk.txt"), "93\n")
	e.write(e.path("ticket.ochan"), `let used = int(trim(read("disk.txt")))
var r = curl("`+server.URL+`/v1/tickets", {"method": "POST", "allow_write": true, "body": {"used": used}})
print(r["status"])
`)
	if out := e.must("plan", "ticket.ochan"); !strings.Contains(out, "запрос     POST "+server.URL+"/v1/tickets") || len(sent()) != 0 {
		t.Fatalf("план: %v\n%s", sent(), out)
	}
	id := requestIDPattern.FindStringSubmatch(e.must("request", "ticket.ochan"))[1]
	e.must("approve", id)
	if out := e.must("apply", "--request", id); !strings.Contains(out, "201") {
		t.Fatalf("apply --request:\n%s", out)
	}
	if s := sent(); len(s) != 1 || s[0] != `POST {"used":93}` {
		t.Fatalf("отправлено: %q", s)
	}

	id = requestIDPattern.FindStringSubmatch(e.must("request", "ticket.ochan"))[1]
	e.must("approve", id)
	e.write(e.path("disk.txt"), "95\n")
	e.fails("тело запроса не то, что согласовали", "apply", "--request", id)
	if s := sent(); len(s) != 1 {
		t.Fatalf("заявка с другим телом отправила запрос: %q", s)
	}
	e.fails(`с "allow_write": true`, "eval", `fetch("`+server.URL+`", {"method": "POST"})`)
}
