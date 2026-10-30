// Package audit — журнал аудита с цепочкой хешей.
//
// Журнал операций (history) отвечает на вопрос «что изменилось и как это
// откатить». Журнал аудита — «кто, что и когда пытался сделать и что ему
// ответили»: каждая команда, вход и смена роли, разрешённые и
// отвергнутые. На общем сервере это то, по чему разбирают инцидент.
//
// Запись — строка JSON. В ней хеш предыдущей записи (prev) и свой (hash) —
// SHA-256 от самой записи без поля hash. Поправить, вставить или удалить
// запись в середине незаметно нельзя: разойдётся цепочка, и Verify
// назовёт первое место разрыва. Обрезать хвост можно, поэтому хеш
// последней записи (Verify его печатает) стоит время от времени сохранять
// вне сервера: Verify с ним проверит, что журнал не короче.
//
// От владельца сервера цепочка не защищает: он может переписать журнал
// целиком, с пересчётом хешей. Её дело — чтобы правку журнала нельзя было
// сделать незаметно для того, кто хранит хеш снаружи.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Genesis — prev первой записи.
var Genesis = strings.Repeat("0", 64)

// Виды событий.
const (
	EventCommand = "command"
	EventLogin   = "login"
	EventRole    = "role"
	// EventTrigger — окно триггера открылось, закрылось или истекло.
	EventTrigger = "trigger"
	// EventFree — free: выдача, отзыв, истечение и решения без человека.
	EventFree = "free"
)

// Record — запись журнала.
type Record struct {
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"time"`
	Event   string    `json:"event"`
	Role    string    `json:"role,omitempty"`
	Command string    `json:"command,omitempty"`
	Args    []string  `json:"args,omitempty"`
	Allowed bool      `json:"allowed"`
	Reason  string    `json:"reason,omitempty"`
	// Remote — адрес клиента сервера; User — пользователь ОС.
	Remote string `json:"remote,omitempty"`
	User   string `json:"user,omitempty"`
	Prev   string `json:"prev"`
	Hash   string `json:"-"`
}

// hashSuffix — хвост строки: ,"hash":"<64 hex>"}.
const hashSuffixLen = len(`,"hash":""}`) + 64

// encode возвращает строку записи и её хеш. Хеш считается от JSON без
// поля hash, а поле дописывается последним: проверка отрезает его и
// считает хеш от того, что осталось, не пересобирая JSON.
func encode(rec Record) ([]byte, string, error) {
	body, err := json.Marshal(rec)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	line := append(body[:len(body)-1:len(body)-1], `,"hash":"`+hash+`"}`...)
	return append(line, '\n'), hash, nil
}

// decode разбирает строку и проверяет её собственный хеш.
func decode(line []byte) (Record, error) {
	var rec Record
	if len(line) < hashSuffixLen+2 || !bytes.HasPrefix(line[len(line)-hashSuffixLen:], []byte(`,"hash":"`)) {
		return rec, errors.New("нет поля hash в конце записи")
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return rec, fmt.Errorf("запись не читается: %w", err)
	}
	split := len(line) - hashSuffixLen
	hash := string(line[split+len(`,"hash":"`) : len(line)-2])
	body := append(append([]byte(nil), line[:split]...), '}')
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != hash {
		return rec, errors.New("хеш записи не сходится: запись правили")
	}
	rec.Hash = hash
	return rec, nil
}

// Journal — журнал в файле.
type Journal struct {
	Path string
	// Now — часы; nil — time.Now.
	Now func() time.Time
}

func (j *Journal) now() time.Time {
	if j.Now != nil {
		return j.Now().UTC()
	}
	return time.Now().UTC()
}

// Recent возвращает записи не старше since, от старых к новым.
type Recent func(since time.Time) ([]Record, error)

// Append дописывает запись. guard, если задан, вызывается под замком
// журнала с доступом к недавним записям — так предел частоты проверяется
// и учитывается одним действием, и два параллельных процесса не пройдут
// оба по последней единице предела. Ошибка guard — отказ: запись
// пишется с Allowed=false и причиной, а ошибка возвращается.
func (j *Journal) Append(rec Record, guard func(Recent) error) error {
	lock, err := lockFile(j.Path + ".lock")
	if err != nil {
		return fmt.Errorf("журнал аудита: %w", err)
	}
	defer lock.unlock()

	last, err := j.last()
	if err != nil {
		return err
	}

	var refusal error
	if guard != nil {
		refusal = guard(func(since time.Time) ([]Record, error) { return j.since(since) })
	}
	rec.Allowed = refusal == nil
	if refusal != nil && rec.Reason == "" {
		rec.Reason = refusal.Error()
	}
	rec.Seq = last.Seq + 1
	rec.Prev = last.Hash
	if rec.Prev == "" {
		rec.Prev = Genesis
	}
	rec.Time = j.now()

	line, _, err := encode(rec)
	if err != nil {
		return fmt.Errorf("журнал аудита: %w", err)
	}
	f, err := os.OpenFile(j.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("журнал аудита: %w", err)
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return fmt.Errorf("журнал аудита: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("журнал аудита: %w", err)
	}
	return refusal
}

// last возвращает последнюю запись; пустой журнал — нулевая запись.
func (j *Journal) last() (Record, error) {
	var last Record
	err := j.tail(func(rec Record) bool {
		last = rec
		return false
	})
	return last, err
}

func (j *Journal) since(since time.Time) ([]Record, error) {
	var records []Record
	err := j.tail(func(rec Record) bool {
		if rec.Time.Before(since) {
			return false
		}
		records = append(records, rec)
		return true
	})
	reverse(records)
	return records, err
}

// tail обходит журнал с конца, от новых записей к старым, пока visit
// говорит «дальше». Журнал растёт, а пределы смотрят на последние часы:
// читать его целиком на каждую команду незачем.
func (j *Journal) tail(visit func(Record) bool) error {
	f, err := os.Open(j.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("журнал аудита: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("журнал аудита: %w", err)
	}

	const block = 64 << 10
	var (
		pending []byte // конец строки, начало которой — в более раннем блоке
		offset  = info.Size()
	)
	for offset > 0 {
		size := min(int64(block), offset)
		offset -= size
		chunk := make([]byte, size, int(size)+len(pending))
		if _, err := f.ReadAt(chunk, offset); err != nil && err != io.EOF {
			return fmt.Errorf("журнал аудита: %w", err)
		}
		chunk = append(chunk, pending...)

		lines := bytes.Split(chunk, []byte{'\n'})
		start := 0
		if offset > 0 {
			// Первая строка блока может быть неполной: дочитается со
			// следующим, более ранним блоком.
			pending, start = lines[0], 1
		}
		for i := len(lines) - 1; i >= start; i-- {
			if len(lines[i]) == 0 {
				continue
			}
			rec, err := decode(lines[i])
			if err != nil {
				return fmt.Errorf("журнал аудита %s повреждён: %w; проверка: ochan audit verify", j.Path, err)
			}
			if !visit(rec) {
				return nil
			}
		}
	}
	return nil
}

func reverse(records []Record) {
	for i, k := 0, len(records)-1; i < k; i, k = i+1, k-1 {
		records[i], records[k] = records[k], records[i]
	}
}

// Summary — итог проверки.
type Summary struct {
	Records     int
	First, Last time.Time
	Head        string
}

// Verify проверяет журнал от начала до конца: хеш каждой записи, ссылку
// на предыдущую и номера. head, если задан, — хеш записи, сохранённый
// когда-то вне сервера (можно начало, от 12 знаков): он должен найтись,
// иначе журнал обрезан или подменён.
func (j *Journal) Verify(head string) (Summary, error) {
	var summary Summary
	f, err := os.Open(j.Path)
	if errors.Is(err, os.ErrNotExist) {
		if head != "" {
			return summary, errors.New("журнала нет, а хеш записи задан: журнал удалён")
		}
		return summary, nil
	}
	if err != nil {
		return summary, err
	}
	defer f.Close()

	if head != "" && len(head) < 12 {
		return summary, errors.New("хеш записи — не короче 12 знаков")
	}
	found := head == ""
	prev, seq := Genesis, int64(0)
	reader := bufio.NewReaderSize(f, 64<<10)
	for line := 1; ; line++ {
		raw, err := reader.ReadBytes('\n')
		if len(raw) == 0 && errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return summary, err
		}
		raw = bytes.TrimSuffix(raw, []byte{'\n'})
		rec, decodeErr := decode(raw)
		switch {
		case decodeErr != nil:
			return summary, fmt.Errorf("строка %d: %w", line, decodeErr)
		case rec.Prev != prev:
			return summary, fmt.Errorf("строка %d: ссылка на предыдущую запись не сходится: "+
				"запись перед ней удалили, вставили или правили", line)
		case rec.Seq != seq+1:
			return summary, fmt.Errorf("строка %d: номер %d, ожидался %d", line, rec.Seq, seq+1)
		}
		if summary.Records == 0 {
			summary.First = rec.Time
		}
		summary.Records++
		summary.Last, summary.Head = rec.Time, rec.Hash
		prev, seq = rec.Hash, rec.Seq
		if !found && strings.HasPrefix(rec.Hash, head) {
			found = true
		}
	}
	if !found {
		return summary, fmt.Errorf("записи %s в журнале нет: журнал обрезан или подменён", head)
	}
	return summary, nil
}

// Last возвращает до n последних записей, от старых к новым; filter, если
// задан, отбирает нужные.
func (j *Journal) Last(n int, filter func(Record) bool) ([]Record, error) {
	var picked []Record
	err := j.tail(func(rec Record) bool {
		if filter == nil || filter(rec) {
			picked = append(picked, rec)
		}
		return n <= 0 || len(picked) < n
	})
	if err != nil {
		return nil, err
	}
	reverse(picked)
	return picked, nil
}
