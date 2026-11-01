package report

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"

	"ochan/config"
)

// openDOCX распаковывает документ и возвращает его части.
func openDOCX(t *testing.T, data []byte) map[string]string {
	t.Helper()

	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("документ не является корректным zip: %v", err)
	}

	parts := make(map[string]string, len(archive.File))
	for _, file := range archive.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("не удалось открыть %s: %v", file.Name, err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("не удалось прочитать %s: %v", file.Name, err)
		}
		parts[file.Name] = string(content)
	}
	return parts
}

// assertWellFormed проверяет, что XML разбирается целиком.
func assertWellFormed(t *testing.T, name, content string) {
	t.Helper()

	decoder := xml.NewDecoder(strings.NewReader(content))
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("%s: некорректный XML: %v", name, err)
		}
	}
}

func samplePlan() *config.Plan {
	return &config.Plan{
		Command: "patch",
		Source:  "changes.conf",
		Target:  "prod.conf",
		Changes: []config.Change{
			{Kind: config.ChangeSet, Target: "prod.conf", Name: "max_connections", From: "100", To: "300"},
			{Kind: config.ChangeAdd, Target: "prod.conf", Name: "autovacuum", To: "on"},
			{Kind: config.ChangeTask, Target: "CI-1", Name: "pangolin_restart", Detail: "confirm=true"},
		},
		Warnings: []string{"требуют перезапуска: shared_buffers"},
	}
}

func TestDOCXHasRequiredParts(t *testing.T) {
	// Минимально корректный .docx — это zip с описанием типов, связями
	// и телом документа. Без любой из частей Word отказывается открывать файл.
	data, err := DOCX(Document{Title: "Заявка", Plan: samplePlan()})
	if err != nil {
		t.Fatalf("DOCX: %v", err)
	}

	parts := openDOCX(t, data)
	for _, required := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml"} {
		if _, ok := parts[required]; !ok {
			t.Errorf("в документе нет части %s", required)
		}
	}

	for name, content := range parts {
		assertWellFormed(t, name, content)
	}
}

func TestDOCXContainsPlanContent(t *testing.T) {
	doc := Document{
		Title:   "Заявка на изменение abc123",
		Created: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		Plan:    samplePlan(),
		Fields: []Field{
			{Name: "Автор", Value: "dba"},
			{Name: "Источник", Value: "changes.conf"},
		},
		Comments: []string{"согласовано в рабочем порядке"},
	}

	data, err := DOCX(doc)
	if err != nil {
		t.Fatalf("DOCX: %v", err)
	}
	body := openDOCX(t, data)["word/document.xml"]

	for _, want := range []string{
		"Заявка на изменение abc123",
		"max_connections", "100", "300",
		"autovacuum",
		"pangolin_restart", "CI-1",
		"требуют перезапуска",
		"согласовано в рабочем порядке",
		"Автор", "dba",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в документе нет %q", want)
		}
	}
}

func TestDOCXEscapesSpecialCharacters(t *testing.T) {
	// Значения конфигов содержат кавычки, амперсанды и угловые скобки —
	// без экранирования документ перестаёт открываться.
	plan := &config.Plan{Changes: []config.Change{{
		Kind:   config.ChangeSet,
		Target: "prod.conf",
		Name:   "log_line_prefix",
		From:   `'%m [%p] <a & b>'`,
		To:     `"<новое> & «значение»"`,
	}}}

	data, err := DOCX(Document{Title: "A & B <тест>", Plan: plan})
	if err != nil {
		t.Fatalf("DOCX: %v", err)
	}

	parts := openDOCX(t, data)
	body := parts["word/document.xml"]

	assertWellFormed(t, "word/document.xml", body)

	if strings.Contains(body, "<a & b>") {
		t.Error("спецсимволы попали в документ неэкранированными")
	}
	if !strings.Contains(body, "&amp;") {
		t.Error("амперсанд не экранирован")
	}
}

func TestDOCXEmptyPlan(t *testing.T) {
	data, err := DOCX(Document{Title: "Пустая заявка"})
	if err != nil {
		t.Fatalf("DOCX без плана вернул ошибку: %v", err)
	}

	body := openDOCX(t, data)["word/document.xml"]
	assertWellFormed(t, "word/document.xml", body)

	if !strings.Contains(body, "Изменений нет") {
		t.Error("отсутствие изменений не отражено в документе")
	}
}

func TestMailConfigValidation(t *testing.T) {
	// Ненастроенная почта — отдельный случай: это не поломка, а отсутствие
	// адресата, и вызывающий должен уметь их различить.
	cases := map[string]MailConfig{
		"нет хоста":       {From: "a@b", To: []string{"c@d"}},
		"нет отправителя": {Host: "smtp", To: []string{"c@d"}},
		"нет получателя":  {Host: "smtp", From: "a@b"},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			err := cfg.Validate()
			if err == nil {
				t.Fatal("неполная настройка принята")
			}
			if !isNotConfiguredErr(err) {
				t.Errorf("ошибка не опознаётся как «не настроено»: %v", err)
			}
		})
	}

	full := MailConfig{Host: "smtp", From: "a@b", To: []string{"c@d"}}
	if err := full.Validate(); err != nil {
		t.Errorf("полная настройка отвергнута: %v", err)
	}
}

func isNotConfiguredErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "почта не настроена")
}

func TestMailMessageEncodesSubjectAndAttachment(t *testing.T) {
	cfg := MailConfig{Host: "smtp", Port: 25, From: "octochan@example", To: []string{"dba@example"}}
	mail := Mail{
		Subject:     "Заявка согласована",
		Body:        "Изменений: 3",
		Attachments: []Attachment{{Name: "заявка.docx", Data: []byte("содержимое")}},
	}

	message := string(buildMessage(cfg, mail))

	// Кириллица в теме обязана быть закодирована, иначе большинство
	// почтовых клиентов показывают мусор.
	if strings.Contains(message, "Subject: Заявка") {
		t.Error("тема письма не закодирована")
	}
	if !strings.Contains(message, "=?utf-8?") {
		t.Error("нет кодировки RFC 2047 в заголовках")
	}

	for _, want := range []string{
		"From: octochan@example",
		"To: dba@example",
		"multipart/mixed",
		"Content-Transfer-Encoding: base64",
		"Изменений: 3",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("в письме нет %q", want)
		}
	}
}
