package embedded

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// Архитектурная модель — своя модель ℒ, у которой логика и кольца в её
// коде, как веса: недостающая предпосылка — её собственный отказ, копию
// задачи и выводимое из посылок пишет её логика, кольцо 2 не пропускает
// слабо поддержанное, сеть выбирает только среди допустимых. Она отвечает
// целиком сама:
//
//	POST /v1/generate ← {"prompt": "…"}
//	                  → {"answer": "…", "refused": false, "packages": […], "questions": […], …}
//
// ochan проверяет такой ответ снаружи и пишет его в журнал решений; сам он
// моделью не управляет. /v1/info говорит о ней "generate": true. Её
// /v1/next тоже есть: распределение уже переопределено её логикой.

// ModelPackage — решение, которое вывела модель: правила, поддержка кольца
// 1, вердикт кольца 2 и, если был выбор, его признаки для калибровки.
// Cluster — кластер повторяющихся инцидентов, по исходам которого взята
// надёжность правил: задача повторяет его структуру.
type ModelPackage struct {
	Statement string   `json:"statement"`
	Rules     []string `json:"rules"`
	L         float64  `json:"l"`
	U         float64  `json:"u"`
	Verdict   string   `json:"verdict"`
	Chosen    bool     `json:"chosen,omitempty"`
	Score     float64  `json:"score,omitempty"`
	LogProb   float64  `json:"logprob,omitempty"`
	Options   int      `json:"options,omitempty"`
	Gap       float64  `json:"gap,omitempty"`
	Cluster   string   `json:"cluster,omitempty"`
}

// ModelQuestion — вопрос оператору: то, что модель сама не решила.
type ModelQuestion struct {
	Attr      string   `json:"attr,omitempty"`
	Targets   []string `json:"targets,omitempty"`
	Values    []string `json:"values,omitempty"`
	Statement string   `json:"statement,omitempty"`
	Rules     []string `json:"rules,omitempty"`
	Cluster   string   `json:"cluster,omitempty"`
	Why       string   `json:"why"`
}

// ModelCluster — кластер повторяющихся инцидентов, структуру которого
// повторяет задача (ochan incident learn, ochan llm learn): частных случаев,
// исходов с весом давности, из них решено моделью самой, и её надёжность на
// этой структуре.
type ModelCluster struct {
	Key       string  `json:"key"`
	Instances int     `json:"instances"`
	Solved    float64 `json:"solved"`
	Outcomes  float64 `json:"outcomes"`
	L         float64 `json:"l"`
	U         float64 `json:"u"`
}

// Answer — ответ архитектурной модели.
type Answer struct {
	Text      string          `json:"answer"`
	Refused   bool            `json:"refused"`
	Missing   []string        `json:"missing,omitempty"`
	Packages  []ModelPackage  `json:"packages,omitempty"`
	Questions []ModelQuestion `json:"questions,omitempty"`
	// Forced — утверждений, которые написала её логика; Chosen — выбранных
	// её сетью среди допустимых.
	Forced  int           `json:"forced"`
	Chosen  int           `json:"chosen"`
	Cluster *ModelCluster `json:"cluster,omitempty"`
}

// Generator — архитектурная модель: отвечает целиком сама.
type Generator interface {
	Generate(ctx context.Context, prompt string) (Answer, error)
}

// GenerateRequest — тело POST /v1/generate.
type GenerateRequest struct {
	Prompt string `json:"prompt"`
}

// Generate — ответ архитектурной модели целиком.
func (h *HTTP) Generate(ctx context.Context, prompt string) (Answer, error) {
	body, _ := json.Marshal(GenerateRequest{Prompt: prompt})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint+"/v1/generate", bytes.NewReader(body))
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	var a Answer
	if err := h.do(req, &a); err != nil {
		return Answer{}, err
	}
	return a, nil
}
