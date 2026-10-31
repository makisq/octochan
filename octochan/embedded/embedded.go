// Package embedded — встроенный режим колец: ochan сам ведёт генерацию
// локальной модели, видит распределение следующего токена и не даёт ей
// напечатать то, что не прошло кольцо 0 или не принадлежит языку.
//
// В режиме протокола модель внешняя: она пишет ответ целиком, кольца
// читают готовый текст, и отвергнутое в нём есть. Здесь иначе:
//
//	модель → распределение следующего токена
//	       → кольцо 0: токен с p ≥ τ, иначе его нет
//	       → маска языка: токен, после которого текст не в языке, не берётся
//	       → утверждение (строка) собрано — проверка языка ℒ;
//	         не прошло — откат к началу утверждения, путь, который увёл из
//	         языка, запрещён
//	       → готовый текст — проверка целиком
//
// Отвергнутое не появляется в ответе вообще. Модель, которую так ведут,
// не может ответить на естественном языке: ни одно утверждение вне ℒ не
// выходит из декодера.
//
// Пакет не знает, какая модель внутри: Engine — распределение следующего
// токена после текста. Своя модель (LLaMA на fp) подключается адаптером к
// протоколу движка (http.go) — это следующий шаг.
package embedded

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"

	"octochan/fp"
	"octochan/rings"
)

// Candidate — токен-кандидат и его вероятность.
type Candidate struct {
	Text string  `json:"text"`
	P    float64 `json:"p"`
}

// Step — распределение следующего токена: кандидаты, самые вероятные
// первыми, и вероятность того, что ответ кончается здесь.
type Step struct {
	Candidates []Candidate `json:"candidates"`
	End        float64     `json:"end"`
}

// Engine — локальная модель.
type Engine interface {
	// Next — распределение следующего токена после prompt+output. top —
	// сколько кандидатов нужно: больше ⌊1/τ⌋ порог τ не пройдёт никогда
	// (rings.MaxPass), поэтому весь словарь движку отдавать не нужно.
	Next(ctx context.Context, prompt, output string, top int) (Step, error)
}

// Config — настройки декодера.
type Config struct {
	// Tau — порог кольца 0.
	Tau float64
	// MaxTokens — сколько раз можно спросить модель за ответ, с откатами.
	MaxTokens int
	// Retries — сколько откатов допустимо на одно утверждение.
	Retries int
	// Statement — что мешает дописать утверждение к тексту (пусто —
	// утверждение в языке). По умолчанию — fp.StatementSyntax.
	Statement func(prefix, statement string) []fp.Diagnostic
	// Final — что мешает принять готовый текст. По умолчанию — ⊥syn
	// fp.CheckModelText.
	Final func(text string) []fp.Diagnostic
}

// Statement — принятое утверждение и средний логарифм вероятности его
// токенов: это β₁ правила выбора (rings.Candidate.MeanLogProb).
type Statement struct {
	Text        string
	MeanLogProb float64
}

// Output — ответ и то, что кольца с ним сделали.
type Output struct {
	Text       string
	Statements []Statement
	// Calls — обращений к модели; Tokens — токенов в ответе.
	Calls, Tokens int
	// Ring0 — кандидатов, отклонённых порогом τ.
	Ring0 int
	// Masked — кандидатов выше порога, отклонённых маской языка на
	// токене: комментарий, символ вне ASCII.
	Masked int
	// OutOfLanguage — утверждений, собранных моделью, но не в языке: их
	// нет в ответе.
	OutOfLanguage int
	// Dead — позиций, где порог не прошёл ни один разрешённый токен.
	Dead int
	// Rollbacks — откатов к началу утверждения.
	Rollbacks int
	// EarlyEnd — попыток закончить ответ раньше, чем текст сложился в язык.
	EarlyEnd int
}

// Failure — ответа нет: декодер не смог провести модель внутри языка.
// Output — что было к этому моменту (без незаконченного утверждения).
type Failure struct {
	Reason string
	Output *Output
}

func (f *Failure) Error() string { return f.Reason }

type step struct {
	prefix string // текст ответа до токена
	text   string // токен, как его предложила модель
	p      float64
}

// Generate ведёт модель от prompt до готового ответа на языке.
func Generate(ctx context.Context, engine Engine, prompt string, cfg Config) (*Output, error) {
	if cfg.Tau <= 0 || cfg.Tau > 1 {
		return nil, fmt.Errorf("порог кольца 0 τ=%g: нужен 0 < τ ≤ 1", cfg.Tau)
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 2048
	}
	if cfg.Retries <= 0 {
		cfg.Retries = 32
	}
	if cfg.Statement == nil {
		cfg.Statement = func(prefix, statement string) []fp.Diagnostic {
			return fp.StatementSyntax(prefix, statement, fp.Options{})
		}
	}
	if cfg.Final == nil {
		cfg.Final = func(text string) []fp.Diagnostic {
			return fp.CheckModelText("model", text, fp.Options{}).Syntax()
		}
	}
	top := rings.MaxPass(cfg.Tau)
	if top > 64 {
		top = 64
	}

	d := &decoder{engine: engine, prompt: prompt, cfg: cfg, top: top, out: &Output{},
		cache: map[string]Step{}, banned: map[string]map[string]bool{}, endBanned: map[string]bool{}}
	return d.run(ctx)
}

type decoder struct {
	engine Engine
	prompt string
	cfg    Config
	top    int
	out    *Output

	cache     map[string]Step
	banned    map[string]map[string]bool
	endBanned map[string]bool
	accepted  string
}

func (d *decoder) fail(format string, args ...any) error {
	d.out.Text = d.accepted
	return &Failure{Reason: fmt.Sprintf(format, args...), Output: d.out}
}

func (d *decoder) ban(s step) {
	if d.banned[s.prefix] == nil {
		d.banned[s.prefix] = map[string]bool{}
	}
	d.banned[s.prefix][s.text] = true
}

func (d *decoder) next(ctx context.Context, prefix string) (Step, error) {
	if s, ok := d.cache[prefix]; ok {
		return s, nil
	}
	if d.out.Calls >= d.cfg.MaxTokens {
		return Step{}, d.fail("модель спрошена %d раз, а ответ не сложился: предел llm.max_tokens", d.out.Calls)
	}
	d.out.Calls++
	s, err := d.engine.Next(ctx, d.prompt, prefix, d.top)
	if err != nil {
		return Step{}, err
	}
	for _, c := range s.Candidates {
		if c.P < d.cfg.Tau {
			d.out.Ring0++
		}
	}
	d.cache[prefix] = s
	return s, nil
}

func (d *decoder) run(ctx context.Context) (*Output, error) {
	var (
		line     string
		path     []step
		attempts int
	)
	restart := func() {
		line, path = "", nil
		d.out.Rollbacks++
		attempts++
	}
	for {
		if attempts > d.cfg.Retries {
			return nil, d.fail("утверждение не сложилось в язык за %d откатов: модель не может продолжить ответ", d.cfg.Retries)
		}
		prefix := d.accepted + line
		s, err := d.next(ctx, prefix)
		if err != nil {
			return nil, err
		}

		// Кольцо 0, затем маска языка на токене.
		var pick *Candidate
		for i := range s.Candidates {
			c := &s.Candidates[i]
			if c.P < d.cfg.Tau || d.banned[prefix][c.Text] {
				continue
			}
			if lexeme(c.Text) {
				d.out.Masked++
				d.ban(step{prefix, c.Text, c.P})
				continue
			}
			if pick == nil || c.P > pick.P {
				pick = c
			}
		}
		end := s.End >= d.cfg.Tau && !d.endBanned[prefix]
		if end && (pick == nil || s.End >= pick.P) {
			text := prefix
			if line != "" && !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			if bad := d.cfg.Final(text); len(bad) > 0 {
				d.out.EarlyEnd++
				d.endBanned[prefix] = true
				continue
			}
			if line != "" {
				d.accept(line+"\n", path)
			}
			d.out.Text = text
			return d.out, nil
		}
		if pick == nil {
			d.out.Dead++
			if len(path) == 0 {
				return nil, d.fail("кольцо 0: в начале утверждения ни один токен не прошёл порог τ=%g — модель не уверена ни в одном продолжении", d.cfg.Tau)
			}
			d.ban(path[len(path)-1])
			restart()
			continue
		}

		// Утверждение кончается на переводе строки: остаток токена после
		// него не берётся — модель предложит его снова уже после строки.
		piece := pick.Text
		if i := strings.IndexByte(piece, '\n'); i >= 0 {
			piece = piece[:i+1]
		}
		path = append(path, step{prefix, pick.Text, pick.P})
		line += piece
		if !strings.HasSuffix(line, "\n") {
			continue
		}

		if bad := d.cfg.Statement(d.accepted, line); len(bad) > 0 {
			d.out.OutOfLanguage++
			d.ban(culprit(path, d.accepted, line, bad[0]))
			restart()
			continue
		}
		d.accept(line, path)
		line, path, attempts = "", nil, 0
	}
}

func (d *decoder) accept(line string, path []step) {
	sum := 0.0
	for _, s := range path {
		sum += math.Log(s.p)
	}
	mean := 0.0
	if len(path) > 0 {
		mean = sum / float64(len(path))
	}
	d.accepted += line
	d.out.Tokens += len(path)
	// Пустая строка — разделитель, а не утверждение.
	if strings.TrimSpace(line) != "" {
		d.out.Statements = append(d.out.Statements, Statement{Text: strings.TrimSuffix(line, "\n"), MeanLogProb: mean})
	}
}

// culprit — токен, который увёл утверждение из языка: тот, что покрывает
// столбец находки. Находка в прежней строке или в начале этой — первый
// токен: само начало утверждения здесь неуместно.
func culprit(path []step, accepted, line string, bad fp.Diagnostic) step {
	lineNo := strings.Count(accepted, "\n") + 1
	if bad.Pos.Line != lineNo || bad.Pos.Col <= 1 {
		return path[0]
	}
	col := 0
	for _, s := range path {
		col += len([]rune(strings.SplitN(s.text, "\n", 2)[0]))
		if col >= bad.Pos.Col {
			return s
		}
	}
	return path[len(path)-1]
}

// lexeme — токен вне языка сам по себе: комментарий, символ вне ASCII,
// управляющий символ, табуляция. Такой токен не берётся, даже если модель
// уверена в нём больше всего.
func lexeme(token string) bool {
	for _, r := range token {
		switch {
		case r == '\n':
		case r == '#', r == '\t', r < ' ', r > unicode.MaxASCII:
			return true
		}
	}
	return false
}

// ErrNoEngine — embedded без движка.
var ErrNoEngine = errors.New("движок встроенной модели не задан: llm.endpoint в config.yaml")
