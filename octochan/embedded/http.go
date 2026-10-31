package embedded

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Протокол движка, версия 1 — то, что адаптер локальной модели (llama.cpp,
// vLLM, своя LLaMA на fp) отдаёт ochan по HTTP:
//
//	GET  /v1/info  → {"protocol": 1, "engine": "llama.cpp", "model": "llama-3.1-8b-q4"}
//	POST /v1/next  ← {"prompt": "…", "output": "…", "top": 2}
//	               → {"candidates": [{"text": "goal", "p": 0.81}, …], "end": 0.001}
//
// prompt — неизменная часть (протокол и задача), output — ответ до этого
// места: адаптер держит prompt в кеше и досчитывает только хвост. top —
// сколько кандидатов нужно: ⌊1/τ⌋, больше порог не пройдёт. p — вероятность
// токена в полном распределении модели (после softmax, до любой выборки:
// температура, top-p и штрафы адаптера не трогают), end — вероятность
// конца ответа. Кандидаты — по убыванию p.

// Protocol — версия протокола движка.
const Protocol = 1

// Info — что о себе говорит движок.
type Info struct {
	Protocol int    `json:"protocol"`
	Engine   string `json:"engine"`
	Model    string `json:"model"`
}

// NextRequest — тело POST /v1/next.
type NextRequest struct {
	Prompt string `json:"prompt"`
	Output string `json:"output"`
	Top    int    `json:"top"`
}

// HTTP — движок по протоколу v1.
type HTTP struct {
	Endpoint string
	Client   *http.Client
}

// NewHTTP — клиент движка по адресу endpoint.
func NewHTTP(endpoint string, timeout time.Duration) *HTTP {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &HTTP{Endpoint: strings.TrimRight(endpoint, "/"), Client: &http.Client{Timeout: timeout}}
}

// Info спрашивает движок о нём самом и сверяет версию протокола.
func (h *HTTP) Info(ctx context.Context) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Endpoint+"/v1/info", nil)
	if err != nil {
		return Info{}, err
	}
	var info Info
	if err := h.do(req, &info); err != nil {
		return Info{}, err
	}
	if info.Protocol != Protocol {
		return info, fmt.Errorf("движок %s говорит по протоколу v%d, ochan — по v%d", h.Endpoint, info.Protocol, Protocol)
	}
	return info, nil
}

// Next — распределение следующего токена.
func (h *HTTP) Next(ctx context.Context, prompt, output string, top int) (Step, error) {
	body, _ := json.Marshal(NextRequest{Prompt: prompt, Output: output, Top: top})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint+"/v1/next", bytes.NewReader(body))
	if err != nil {
		return Step{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	var s Step
	if err := h.do(req, &s); err != nil {
		return Step{}, err
	}
	return s, s.validate()
}

func (h *HTTP) do(req *http.Request, into any) error {
	resp, err := h.Client.Do(req)
	if err != nil {
		return fmt.Errorf("движок %s недоступен: %v", h.Endpoint, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("движок %s: %s: %s", h.Endpoint, resp.Status, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("движок %s ответил не по протоколу v%d: %v", h.Endpoint, Protocol, err)
	}
	return nil
}

// validate — распределение похоже на распределение: вероятности в [0, 1]
// и в сумме не больше единицы. Иначе порог τ сравнивался бы не с тем.
func (s Step) validate() error {
	sum := s.End
	for _, c := range s.Candidates {
		if c.P < 0 || c.P > 1 {
			return fmt.Errorf("движок дал вероятность %g токену %q: нужна от 0 до 1", c.P, c.Text)
		}
		sum += c.P
	}
	if s.End < 0 || sum > 1+1e-6 {
		return fmt.Errorf("вероятности движка в сумме %.4f: это не распределение (температура или штрафы адаптера?)", sum)
	}
	return nil
}

// Handler — сервер протокола v1 вокруг движка: адаптер локальной модели
// пишется как Engine и выставляется этим обработчиком.
func Handler(info Info, engine Engine) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/v1/info", func(w http.ResponseWriter, r *http.Request) {
		info.Protocol = Protocol
		reply(w, http.StatusOK, info)
	})
	mux.HandleFunc("/v1/next", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			reply(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST"})
			return
		}
		var req NextRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s, err := engine.Next(r.Context(), req.Prompt, req.Output, req.Top)
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		reply(w, http.StatusOK, s)
	})
	return mux
}
