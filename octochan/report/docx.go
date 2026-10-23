// Package report — представления плана и журнала для людей и внешних систем.
//
// План и запись истории уже описывают изменение целиком, поэтому документ
// и письмо здесь — не отдельные возможности, а способы отрисовать одну
// и ту же модель. Если сделать наоборот, каждый новый формат отчёта
// превращается в переписывание склеенных строк.
package report

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"octochan/config"
)

// Document — заявка на изменение.
type Document struct {
	Title    string
	Author   string
	Created  time.Time
	Fields   []Field
	Plan     *config.Plan
	Comments []string
	// Listing — текст сценария, если согласуют сценарий на языке алиасов:
	// подписывают не только план, но и то, что его породило.
	Listing string
	// Prompt — формальный промпт, по которому модель написала сценарий;
	// Report — решения, которые модель приняла сама.
	Prompt string
	Report []string
}

// Field — строка сводной таблицы в шапке документа.
type Field struct {
	Name  string
	Value string
}

// DOCX собирает документ в формате Word.
//
// Файл .docx — это zip с несколькими частями XML, поэтому он пишется
// напрямую через archive/zip и encoding/xml: библиотека для этого
// не нужна, а движок сравнения конфигов принципиально живёт без
// внешних зависимостей, и отчётам незачем их тащить.
func DOCX(doc Document) ([]byte, error) {
	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)

	parts := []struct {
		name    string
		content string
	}{
		{"[Content_Types].xml", contentTypesXML},
		{"_rels/.rels", packageRelsXML},
		{"word/document.xml", documentXML(doc)},
	}

	for _, part := range parts {
		writer, err := archive.Create(part.name)
		if err != nil {
			return nil, fmt.Errorf("не удалось создать часть %s: %w", part.name, err)
		}
		if _, err := writer.Write([]byte(part.content)); err != nil {
			return nil, fmt.Errorf("не удалось записать часть %s: %w", part.name, err)
		}
	}

	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("не удалось закрыть документ: %w", err)
	}
	return buf.Bytes(), nil
}

const contentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>`

const packageRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`

const wordNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

func documentXML(doc Document) string {
	var b strings.Builder

	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	fmt.Fprintf(&b, `<w:document %s><w:body>`, wordNS)

	title := doc.Title
	if title == "" {
		title = "Заявка на изменение"
	}
	b.WriteString(heading(title, 32))

	if len(doc.Fields) > 0 {
		rows := make([][]string, 0, len(doc.Fields))
		for _, field := range doc.Fields {
			rows = append(rows, []string{field.Name, field.Value})
		}
		b.WriteString(table(nil, rows))
		b.WriteString(paragraph(""))
	}

	b.WriteString(heading("Состав изменений", 26))
	b.WriteString(planTable(doc.Plan))

	if doc.Plan != nil && len(doc.Plan.Warnings) > 0 {
		b.WriteString(paragraph(""))
		b.WriteString(heading("Предупреждения", 26))
		for _, warning := range doc.Plan.Warnings {
			b.WriteString(paragraph("— " + warning))
		}
	}

	if doc.Listing != "" {
		b.WriteString(paragraph(""))
		b.WriteString(heading("Текст сценария", 26))
		// Строка на абзац: перевод строки внутри w:t Word показывает пробелом.
		for _, line := range strings.Split(strings.TrimRight(doc.Listing, "\n"), "\n") {
			b.WriteString(codeParagraph(strings.TrimRight(line, "\r")))
		}
	}

	if doc.Prompt != "" {
		b.WriteString(paragraph(""))
		b.WriteString(heading("Формальный промпт", 26))
		for _, line := range strings.Split(strings.TrimRight(doc.Prompt, "\n"), "\n") {
			b.WriteString(codeParagraph(strings.TrimRight(line, "\r")))
		}
	}

	if len(doc.Report) > 0 {
		b.WriteString(paragraph(""))
		b.WriteString(heading("Отчёт модели", 26))
		for _, line := range doc.Report {
			b.WriteString(paragraph("— " + line))
		}
	}

	if len(doc.Comments) > 0 {
		b.WriteString(paragraph(""))
		b.WriteString(heading("Комментарии", 26))
		for _, comment := range doc.Comments {
			b.WriteString(paragraph("— " + comment))
		}
	}

	// Размер страницы А4 в двадцатых долях пункта: без sectPr Word
	// подставляет Letter, и документ печатается не туда.
	b.WriteString(`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/>` +
		`<w:pgMar w:top="1134" w:right="850" w:bottom="1134" w:left="1701"/></w:sectPr>`)
	b.WriteString(`</w:body></w:document>`)

	return b.String()
}

// planTable раскладывает изменения плана в таблицу.
func planTable(plan *config.Plan) string {
	if plan == nil || plan.Len() == 0 {
		return paragraph("Изменений нет.")
	}

	rows := make([][]string, 0, plan.Len())
	for _, change := range plan.Changes {
		rows = append(rows, []string{
			change.Target,
			change.Kind.Russian(),
			change.Name,
			changeValue(change),
		})
	}
	return table([]string{"Цель", "Действие", "Параметр", "Значение"}, rows)
}

func changeValue(change config.Change) string {
	switch change.Kind {
	case config.ChangeAdd:
		return change.To
	case config.ChangeRemove:
		return "было: " + change.From
	case config.ChangeTask, config.ChangeWrite:
		return change.Detail
	default:
		return fmt.Sprintf("%s → %s", change.From, change.To)
	}
}

func heading(text string, halfPoints int) string {
	return fmt.Sprintf(
		`<w:p><w:pPr><w:spacing w:before="240" w:after="120"/></w:pPr>`+
			`<w:r><w:rPr><w:b/><w:sz w:val="%d"/></w:rPr><w:t xml:space="preserve">%s</w:t></w:r></w:p>`,
		halfPoints, escape(text))
}

func paragraph(text string) string {
	return fmt.Sprintf(`<w:p><w:r><w:t xml:space="preserve">%s</w:t></w:r></w:p>`, escape(text))
}

// codeParagraph — строка кода моноширинным шрифтом без отступов между строками.
func codeParagraph(text string) string {
	return fmt.Sprintf(`<w:p><w:pPr><w:spacing w:before="0" w:after="0"/></w:pPr>`+
		`<w:r><w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:cs="Consolas"/><w:sz w:val="18"/></w:rPr>`+
		`<w:t xml:space="preserve">%s</w:t></w:r></w:p>`, escape(text))
}

// table строит таблицу с рамками; пустой header означает таблицу без шапки.
func table(header []string, rows [][]string) string {
	var b strings.Builder

	b.WriteString(`<w:tbl><w:tblPr><w:tblW w:w="0" w:type="auto"/><w:tblBorders>`)
	for _, side := range []string{"top", "left", "bottom", "right", "insideH", "insideV"} {
		fmt.Fprintf(&b, `<w:%s w:val="single" w:sz="4" w:color="999999"/>`, side)
	}
	b.WriteString(`</w:tblBorders></w:tblPr>`)

	if len(header) > 0 {
		b.WriteString(tableRow(header, true))
	}
	for _, row := range rows {
		b.WriteString(tableRow(row, false))
	}

	b.WriteString(`</w:tbl>`)
	return b.String()
}

func tableRow(cells []string, bold bool) string {
	var b strings.Builder
	b.WriteString(`<w:tr>`)

	for _, cell := range cells {
		b.WriteString(`<w:tc><w:tcPr><w:tcW w:w="0" w:type="auto"/></w:tcPr><w:p><w:r>`)
		if bold {
			b.WriteString(`<w:rPr><w:b/></w:rPr>`)
		}
		fmt.Fprintf(&b, `<w:t xml:space="preserve">%s</w:t></w:r></w:p></w:tc>`, escape(cell))
	}

	b.WriteString(`</w:tr>`)
	return b.String()
}

// escape экранирует текст для XML.
//
// Значения конфигов содержат кавычки, амперсанды и угловые скобки
// (log_line_prefix, строки подключения), и без экранирования документ
// перестаёт открываться.
func escape(text string) string {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(text)); err != nil {
		// EscapeText в bytes.Buffer не отказывает; ветка на случай
		// будущей смены приёмника.
		return ""
	}
	return b.String()
}
