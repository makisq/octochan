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

// Протокол движка, версия 1 — то, что модель по адресу отдаёт ochan по HTTP:
// своя модель ℒ другой машины (ochan llm serve) или чужой движок.
//
//	GET  /v1/info  → {"protocol": 1, "engine": "ochan", "model": "~/models/own", "generate": true}
//	POST /v1/next  ← {"prompt": "…", "output": "…", "top": 2}
//	               → {"candidates": [{"text": "goal", "p": 0.81}, …], "end": 0.001}
//
// prompt — неизменная часть (протокол и задача), output — ответ до этого
// места: адаптер держит prompt в кеше и досчитывает только хвост. top —
// сколько кандидатов нужно: ⌊1/τ⌋, больше порог не пройдёт. p — вероятность
// токена в полном распределении модели (после softmax, до любой выборки:
// температура, top-p и штрафы адаптера не трогают), end — вероятность
// конца ответа. Кандидаты — по убыванию p.
//
// Где логика ochan оставляет выбор из нескольких утверждений, модель
// только оценивает их целиком:
//
//	POST /v1/score ← {"prompt": "…", "output": "…", "continuations": ["  set A.x = 1\n", …]}
//	               → {"scores": [{"logp": -0.21, "tokens": 7}, …]}
//
// logp — сумма log p токенов продолжения после prompt+output, в том же
// полном распределении, что у /v1/next; tokens — их число. Порядок — как
// в запросе. Движок без /v1/score подходит, пока выбора нет.
//
// output — всегда прежний output (или начало строки после отката),
// продлённый текстом кандидата, которого этот движок предложил: адаптер
// может кодировать его теми же токенами, а не токенизировать заново — BPE
// на стыке даёт другие («\n\n» одним токеном вместо двух «\n»). Декодер
// проверяет язык по строкам и обрезает кандидата на «\n»: текст после
// перевода строки в том же токене теряется. Модель для встроенного режима
// таких токенов не предлагает — у своей модели их нет в алфавите
// (llama/prune.py).

// Protocol — версия протокола движка.
const Protocol = 1

// Info — что о себе говорит движок. Generate — модель архитектурная и
// отвечает целиком сама (arch.go). Knowledge — пакеты знаний, которые она
// видит: «имя версия».
type Info struct {
	Protocol  int      `json:"protocol"`
	Engine    string   `json:"engine"`
	Model     string   `json:"model"`
	Generate  bool     `json:"generate,omitempty"`
	Knowledge []string `json:"knowledge,omitempty"`
}

// NextRequest — тело POST /v1/next.
type NextRequest struct {
	Prompt string `json:"prompt"`
	Output string `json:"output"`
	Top    int    `json:"top"`
}

// ScoreRequest — тело POST /v1/score.
type ScoreRequest struct {
	Prompt        string   `json:"prompt"`
	Output        string   `json:"output"`
	Continuations []string `json:"continuations"`
}

// ScoreReply — ответ POST /v1/score.
type ScoreReply struct {
	Scores []Scored `json:"scores"`
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

// Score — оценки продолжений целиком.
func (h *HTTP) Score(ctx context.Context, prompt, output string, continuations []string) ([]Scored, error) {
	body, _ := json.Marshal(ScoreRequest{Prompt: prompt, Output: output, Continuations: continuations})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint+"/v1/score", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	var r ScoreReply
	if err := h.do(req, &r); err != nil {
		return nil, err
	}
	if len(r.Scores) != len(continuations) {
		return nil, fmt.Errorf("движок %s оценил %d продолжений из %d", h.Endpoint, len(r.Scores), len(continuations))
	}
	for i, s := range r.Scores {
		if s.LogP > 1e-6 || s.Tokens < 1 {
			return nil, fmt.Errorf("движок %s: оценка продолжения %q — logp %g, токенов %d: нужно logp ≤ 0 и хотя бы один токен",
				h.Endpoint, continuations[i], s.LogP, s.Tokens)
		}
	}
	return r.Scores, nil
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
	if resp.StatusCode == http.StatusNotFound && strings.HasSuffix(req.URL.Path, "/v1/score") {
		return fmt.Errorf("движок %s не оценивает продолжения (нет /v1/score), а логика оставила выбор", h.Endpoint)
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

// Handler — сервер протокола v1 вокруг движка: своя модель (ochan llm
// serve) или демонстрационный движок выставляются этим обработчиком.
func Handler(info Info, engine Engine) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	generator, architectural := engine.(Generator)
	mux.HandleFunc("/v1/info", func(w http.ResponseWriter, r *http.Request) {
		info.Protocol = Protocol
		info.Generate = architectural
		reply(w, http.StatusOK, info)
	})
	if architectural {
		mux.HandleFunc("/v1/generate", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				reply(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST"})
				return
			}
			var req GenerateRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
				reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			a, err := generator.Generate(r.Context(), req.Prompt)
			if err != nil {
				reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			reply(w, http.StatusOK, a)
		})
	}
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
	if scorer, ok := engine.(Scorer); ok {
		mux.HandleFunc("/v1/score", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				reply(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST"})
				return
			}
			var req ScoreRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
				reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			scores, err := scorer.Score(r.Context(), req.Prompt, req.Output, req.Continuations)
			if err != nil {
				reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			reply(w, http.StatusOK, ScoreReply{Scores: scores})
		})
	}
	return mux
}
