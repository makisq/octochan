package remote

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type haNode struct {
	ha   *HA
	base string
	srv  *httptest.Server
}

func (n *haNode) write(t *testing.T, rel, text string) {
	t.Helper()
	if err := writeAtomic(filepath.Join(n.base, rel), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (n *haNode) read(rel string) string {
	data, err := os.ReadFile(filepath.Join(n.base, rel))
	if err != nil {
		return "<нет>"
	}
	return string(data)
}

// haPair поднимает два узла на 127.0.0.1: a — primary, b — standby.
func haPair(t *testing.T, require bool) (a, b *haNode) {
	t.Helper()
	nodes := []*haNode{{base: t.TempDir()}, {base: t.TempDir()}}
	var handlers [2]http.Handler
	var mu sync.Mutex
	for i := range nodes {
		i := i
		nodes[i].srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			h := handlers[i]
			mu.Unlock()
			h.ServeHTTP(w, r)
		}))
	}
	for i, n := range nodes {
		role := RolePrimary
		if i == 1 {
			role = RoleStandby
		}
		state := filepath.Join(n.base, "ha.json")
		if err := SaveHAState(state, &HAState{Node: string(rune('a' + i)), Role: role, Epoch: 1}); err != nil {
			t.Fatal(err)
		}
		ha, err := NewHA(HAConfig{StatePath: state, Base: n.base, Paths: []string{"requests", "roles.bin", "audit.log"},
			Self: n.srv.Listener.Addr().String(), Peer: nodes[1-i].srv.Listener.Addr().String(),
			Token: "секрет-пары", RequireStandby: require})
		if err != nil {
			t.Fatal(err)
		}
		n.ha = ha
		mux := http.NewServeMux()
		ha.register(mux)
		mu.Lock()
		handlers[i] = mux
		mu.Unlock()
	}
	for _, n := range nodes {
		n.srv.Start()
		t.Cleanup(n.srv.Close)
	}
	return nodes[0], nodes[1]
}

func TestHAReplicationAndFailover(t *testing.T) {
	ctx := context.Background()
	a, b := haPair(t, false)
	a.write(t, "requests/r1.json", `{"id":"r1"}`)
	a.write(t, "requests/r1.lock", "")
	a.write(t, "roles.bin", "роли")
	a.write(t, "config.yaml", "свой конфиг узла")
	b.write(t, "requests/stale.json", "лишнее")

	b.ha.step(ctx)
	if b.read("requests/r1.json") != `{"id":"r1"}` || b.read("roles.bin") != "роли" {
		t.Fatalf("копия не дошла: %q %q (%s)", b.read("requests/r1.json"), b.read("roles.bin"), b.ha.Status().Error)
	}
	if b.read("config.yaml") != "<нет>" || b.read("requests/r1.lock") != "<нет>" {
		t.Fatal("скопировано то, что не состояние")
	}
	if b.read("requests/stale.json") != "<нет>" {
		t.Fatal("файл, которого у primary нет, остался у standby")
	}
	if b.ha.Writable() || !a.ha.Writable() {
		t.Fatal("запись: primary и standby перепутаны")
	}

	// Барьер: пометка primary доходит до standby сразу.
	a.write(t, "requests/r1.json", `{"id":"r1","state":"running"}`)
	if warning, err := a.ha.Barrier(ctx); err != nil || warning != "" {
		t.Fatalf("барьер: %q %v", warning, err)
	}
	if !strings.Contains(b.read("requests/r1.json"), "running") {
		t.Fatal("барьер прошёл, а пометки у standby нет")
	}
	if _, err := b.ha.Barrier(ctx); err == nil || !strings.Contains(err.Error(), "не primary") {
		t.Fatalf("барьер на standby: %v", err)
	}

	// Чужой секрет и путь за пределы — отказ.
	for path, want := range map[string]int{"/v1/ha/manifest": 401, "/v1/ha/file?path=../../etc/passwd": 403} {
		req, _ := http.NewRequest(http.MethodGet, a.srv.URL+path, nil)
		token := "Pair чужой"
		if want == 403 {
			token = "Pair секрет-пары"
		}
		req.Header.Set("Authorization", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}

	// Разрыв: a пишет то, чего b не видел; b повышают.
	a.write(t, "requests/r2.json", "записано после разрыва")
	st, err := Promote(filepath.Join(b.base, "ha.json"), "дежурный", a.ha.Status().Epoch)
	if err != nil || st.Epoch != 2 {
		t.Fatalf("promote: %+v %v", st, err)
	}
	if err := b.ha.reload(true); err != nil {
		t.Fatal(err)
	}
	b.ha.step(ctx) // b видит отставшего primary и говорит ему свою эпоху
	as := a.ha.Status()
	if as.Role != RoleStandby || as.Epoch != 2 || len(as.Diverged) != 1 {
		t.Fatalf("бывший primary не отступил: %+v", as)
	}
	if a.read("requests/r2.json") != "<нет>" ||
		a.read(filepath.Join(as.Diverged[0], "requests/r2.json")) != "записано после разрыва" {
		t.Fatal("расходящееся состояние не отложено")
	}
	if !b.ha.Writable() || a.ha.Writable() {
		t.Fatal("после переключения запись не на новом primary")
	}

	// Два primary одной эпохи — запись закрыта на обоих.
	if err := SaveHAState(filepath.Join(a.base, "ha.json"), &HAState{Node: "a", Role: RolePrimary, Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	_ = a.ha.reload(true)
	a.ha.step(ctx)
	b.ha.step(ctx)
	if a.ha.Writable() || b.ha.Writable() || !strings.Contains(a.ha.Status().Error, "конфликт") {
		t.Fatalf("конфликт: a %+v, b %+v", a.ha.Status(), b.ha.Status())
	}
}

func TestHABarrierWithoutStandby(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	for _, require := range []bool{false, true} {
		base := t.TempDir()
		state := filepath.Join(base, "ha.json")
		_ = SaveHAState(state, &HAState{Node: "a", Role: RolePrimary, Epoch: 1})
		ha, err := NewHA(HAConfig{StatePath: state, Base: base, Paths: []string{"requests"}, Peer: dead,
			Token: "t", RequireStandby: require, Every: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		warning, err := ha.Barrier(context.Background())
		if require && (err == nil || !strings.Contains(err.Error(), "ha.require_standby")) {
			t.Fatalf("require: %v", err)
		}
		if !require && (err != nil || !strings.Contains(warning, "выполнит заявку ещё раз")) {
			t.Fatalf("без require: %q %v", warning, err)
		}
	}
	if _, err := NewHA(HAConfig{StatePath: "x", Token: ""}); err == nil {
		t.Fatal("пара без секрета")
	}
}
