package report

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// MailConfig — настройки отправки почты.
type MailConfig struct {
	Host     string
	Port     int
	From     string
	To       []string
	Username string
	Password string
	// StartTLS требует шифрования: если сервер не предлагает STARTTLS,
	// письмо не отправляется вовсе.
	//
	// Само по себе шифрование включается всегда, когда сервер его предлагает.
	// Этот флаг отвечает на другой вопрос — что делать, когда не предлагает:
	// по умолчанию письмо уходит открытым текстом, а с ним не уходит никак.
	StartTLS bool
}

// ErrNoTLS означает, что сервер не поддерживает шифрование,
// а настройки требуют его обязательно.
var ErrNoTLS = errors.New("сервер не предлагает STARTTLS")

// ErrMailNotConfigured означает, что почта не настроена.
//
// Отдельная ошибка нужна вызывающему: «письмо не отправлено, потому что
// некуда» и «письмо не ушло из-за сбоя» — разные события, и первое
// не должно выглядеть как поломка.
var ErrMailNotConfigured = errors.New("почта не настроена")

// Validate проверяет достаточность настроек.
func (c MailConfig) Validate() error {
	switch {
	case c.Host == "":
		return fmt.Errorf("%w: не задан defaults.smtp_host", ErrMailNotConfigured)
	case c.From == "":
		return fmt.Errorf("%w: не задан defaults.smtp_from", ErrMailNotConfigured)
	case len(c.To) == 0:
		return fmt.Errorf("%w: не задан defaults.smtp_to", ErrMailNotConfigured)
	}
	return nil
}

func (c MailConfig) address() string {
	port := c.Port
	if port == 0 {
		port = 25
	}
	return net.JoinHostPort(c.Host, fmt.Sprint(port))
}

// Attachment — вложение письма.
type Attachment struct {
	Name string
	Data []byte
}

// Mail — письмо с необязательными вложениями.
type Mail struct {
	Subject     string
	Body        string
	Attachments []Attachment
}

// Send отправляет письмо.
func Send(cfg MailConfig, mail Mail) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	if err := deliver(cfg, buildMessage(cfg, mail)); err != nil {
		return fmt.Errorf("не удалось отправить письмо через %s: %w", cfg.address(), err)
	}
	return nil
}

// deliver проводит сеанс SMTP.
//
// Диалог ведётся вручную, а не через smtp.SendMail, ровно ради одного шага —
// проверки StartTLS. SendMail шифрует, когда сервер предлагает, и молча
// отправляет открытым текстом, когда не предлагает; узнать, как именно ушло
// письмо, вызывающий не может.
func deliver(cfg MailConfig, message []byte) error {
	client, err := smtp.Dial(cfg.address())
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Hello("localhost"); err != nil {
		return err
	}

	switch ok, _ := client.Extension("STARTTLS"); {
	case ok:
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return err
		}
	case cfg.StartTLS:
		return fmt.Errorf("%w: письмо не отправлено, чтобы не уйти открытым текстом "+
			"(снимите defaults.smtp_starttls, если это приемлемо)", ErrNoTLS)
	}

	if cfg.Username != "" {
		// PlainAuth сам откажется передавать пароль по незашифрованному
		// соединению — пароль не утечёт даже при ошибке в настройках.
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return err
		}
	}

	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	for _, to := range cfg.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("получатель %s: %w", to, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(message); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	return client.Quit()
}

// boundary разделяет части составного письма.
const boundary = "octochan-part-boundary"

func buildMessage(cfg MailConfig, mail Mail) []byte {
	var b strings.Builder

	fmt.Fprintf(&b, "From: %s\r\n", cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(cfg.To, ", "))
	// Тема кодируется по RFC 2047: без этого кириллица в теме приходит
	// нечитаемой в большинстве почтовых клиентов.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", mail.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")

	if len(mail.Attachments) == 0 {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		b.WriteString(mail.Body)
		return []byte(b.String())
	}

	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(mail.Body)
	b.WriteString("\r\n")

	for _, attachment := range mail.Attachments {
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		b.WriteString("Content-Type: application/octet-stream\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n")
		fmt.Fprintf(&b, "Content-Disposition: attachment; filename=%q\r\n\r\n",
			mime.QEncoding.Encode("utf-8", attachment.Name))

		encoded := base64.StdEncoding.EncodeToString(attachment.Data)
		// Строки base64 в письме принято резать по 76 символов.
		for len(encoded) > 76 {
			b.WriteString(encoded[:76])
			b.WriteString("\r\n")
			encoded = encoded[76:]
		}
		b.WriteString(encoded)
		b.WriteString("\r\n")
	}

	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String())
}
