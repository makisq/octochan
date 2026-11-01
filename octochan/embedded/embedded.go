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
// Часть ответа модель не пишет вовсе. Начало (Config.Prefix — копию
// задачи) пишет сам ochan. Там, где ведёт логика (Config.Packages),
// единица ответа — пакет, утверждение целиком: логика называет допустимые;
// один — его пишет декодер, несколько — модель только выбирает среди них
// (Scorer), ни одного — ответ кончается. Статистика решает только там, где
// посылки оставляют выбор.
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

// Scored — оценка продолжения: сумма log p его токенов и их число.
type Scored struct {
	LogP   float64 `json:"logp"`
	Tokens int     `json:"tokens"`
}

// Scorer — движок, который оценивает продолжения целиком: так модель
// выбирает среди пакетов, допустимых по логике.
type Scorer interface {
	Score(ctx context.Context, prompt, output string, continuations []string) ([]Scored, error)
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
	// Prefix — начало ответа, которое пишет сам ochan (копия задачи); оно
	// кончается переводом строки. Модель продолжает после него.
	Prefix string
	// Packages — логика: допустимые следующие утверждения (с переводом
	// строки) после output. ok ложно — это место логика не ведёт, и
	// утверждение пишет модель по токену. Пустой список при ok — ответ
	// кончается здесь.
	Packages func(output string) (adm []string, ok bool)
}

// Statement — принятое утверждение и средний логарифм вероятности его
// токенов: это β₁ правила выбора (rings.Candidate.MeanLogProb). Forced —
// утверждение написала логика: оно было единственным допустимым. Chosen —
// модель выбрала его среди допустимых.
type Statement struct {
	Text        string
	MeanLogProb float64
	Forced      bool
	Chosen      bool
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
	// Forced — утверждений, которые написала логика (единственные
	// допустимые); Chosen — выбранных моделью среди допустимых.
	Forced, Chosen int
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

	if cfg.Prefix != "" && !strings.HasSuffix(cfg.Prefix, "\n") {
		cfg.Prefix += "\n"
	}
	d := &decoder{engine: engine, prompt: prompt, cfg: cfg, top: top, out: &Output{},
		cache: map[string]Step{}, banned: map[string]map[string]bool{}, endBanned: map[string]bool{},
		accepted: cfg.Prefix}
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
		return Step{}, d.fail("модель спрошена %d раз, а ответ не сложился: предел llm.decoder.max_tokens", d.out.Calls)
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
		if line == "" && d.cfg.Packages != nil {
			if adm, ok := d.cfg.Packages(d.accepted); ok {
				done, err := d.packages(ctx, adm)
				if err != nil {
					return nil, err
				}
				if done {
					return d.out, nil
				}
				continue
			}
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
	d.out.Tokens += len(path)
	d.add(Statement{Text: line, MeanLogProb: mean})
}

// add дописывает утверждение к ответу. Пустая строка — разделитель, а не
// утверждение.
func (d *decoder) add(s Statement) {
	d.accepted += s.Text
	if strings.TrimSpace(s.Text) != "" {
		s.Text = strings.TrimSuffix(s.Text, "\n")
		d.out.Statements = append(d.out.Statements, s)
	}
}

// packages — место, которое ведёт логика: adm — допустимые утверждения.
// Возвращает done, когда ответ закончен.
func (d *decoder) packages(ctx context.Context, adm []string) (bool, error) {
	if len(adm) == 0 {
		if bad := d.cfg.Final(d.accepted); len(bad) > 0 {
			return false, d.fail("логика закончила ответ, а он не в языке: %s", bad[0])
		}
		d.out.Text = d.accepted
		return true, nil
	}
	s := Statement{Text: adm[0], Forced: true}
	if len(adm) > 1 {
		scorer, ok := d.engine.(Scorer)
		if !ok {
			return false, d.fail("логика оставила выбор из %d утверждений, а движок их не оценивает (нет /v1/score)", len(adm))
		}
		if d.out.Calls >= d.cfg.MaxTokens {
			return false, d.fail("модель спрошена %d раз, а ответ не сложился: предел llm.decoder.max_tokens", d.out.Calls)
		}
		d.out.Calls++
		scores, err := scorer.Score(ctx, d.prompt, d.accepted, adm)
		if err != nil {
			return false, err
		}
		if len(scores) != len(adm) {
			return false, fmt.Errorf("движок оценил %d продолжений из %d", len(scores), len(adm))
		}
		best := 0
		for i := range scores {
			if scores[i].LogP > scores[best].LogP {
				best = i
			}
		}
		s = Statement{Text: adm[best], MeanLogProb: scores[best].LogP / float64(max(1, scores[best].Tokens)), Chosen: true}
	}
	if bad := d.cfg.Statement(d.accepted, s.Text); len(bad) > 0 {
		return false, d.fail("логика допустила утверждение не в языке: %s", bad[0])
	}
	if s.Chosen {
		d.out.Chosen++
	} else {
		d.out.Forced++
	}
	d.add(s)
	return false, nil
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
