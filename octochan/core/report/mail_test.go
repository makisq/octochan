package report

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeSMTP — минимальный SMTP-сервер для тестов.
//
// Проверять письмо только по его тексту недостаточно: половина ошибок
// отправки живёт не в письме, а в диалоге с сервером — не тот отправитель
// в MAIL FROM, потерянный получатель, незакрытый DATA. Поэтому сеанс
// проводится по-настоящему, через петлю на localhost.
type fakeSMTP struct {
	listener net.Listener
	wg       sync.WaitGroup

	// extensions перечисляется в ответе на EHLO.
	extensions []string

	mu       sync.Mutex
	from     string
	rcpt     []string
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T, extensions ...string) *fakeSMTP {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("не удалось открыть порт: %v", err)
	}

	server := &fakeSMTP{listener: listener, extensions: extensions}
	server.wg.Add(1)
	go server.serve()

	t.Cleanup(func() {
		listener.Close()
		server.wg.Wait()
	})
	return server
}

// addr возвращает хост и порт, на которых слушает сервер.
func (s *fakeSMTP) addr() (string, int) {
	a := s.listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (s *fakeSMTP) serve() {
	defer s.wg.Done()

	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	write := func(line string) { conn.Write([]byte(line + "\r\n")) }

	write("220 fake ESMTP")

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		s.mu.Lock()
		s.commands = append(s.commands, line)
		s.mu.Unlock()

		verb := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(verb, "EHLO"):
			// Последняя строка многострочного ответа отделяется пробелом.
			for _, ext := range s.extensions {
				write("250-" + ext)
			}
			write("250 OK")

		case strings.HasPrefix(verb, "HELO"):
			write("250 OK")

		case strings.HasPrefix(verb, "MAIL FROM:"):
			s.mu.Lock()
			s.from = strings.TrimPrefix(line, "MAIL FROM:")
			s.mu.Unlock()
			write("250 OK")

		case strings.HasPrefix(verb, "RCPT TO:"):
			s.mu.Lock()
			s.rcpt = append(s.rcpt, strings.TrimPrefix(line, "RCPT TO:"))
			s.mu.Unlock()
			write("250 OK")

		case verb == "DATA":
			write("354 давайте")
			var body strings.Builder
			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if dataLine == ".\r\n" {
					break
				}
				body.WriteString(dataLine)
			}
			s.mu.Lock()
			s.data = body.String()
			s.mu.Unlock()
			write("250 OK")

		case verb == "QUIT":
			write("221 пока")
			return

		default:
			write("500 не понял")
		}
	}
}

func (s *fakeSMTP) received() (from string, rcpt []string, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.from, append([]string(nil), s.rcpt...), s.data
}

func (s *fakeSMTP) sawCommand(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cmd := range s.commands {
		if strings.HasPrefix(strings.ToUpper(cmd), prefix) {
			return true
		}
	}
	return false
}

func TestSendCompletesSMTPSession(t *testing.T) {
	server := newFakeSMTP(t)
	host, port := server.addr()

	cfg := MailConfig{
		Host: host,
		Port: port,
		From: "octochan@example",
		To:   []string{"dba@example", "duty@example"},
	}
	mail := Mail{
		Subject:     "Заявка согласована",
		Body:        "Изменений: 2",
		Attachments: []Attachment{{Name: "заявка.docx", Data: []byte("PK\x03\x04содержимое")}},
	}

	if err := Send(cfg, mail); err != nil {
		t.Fatalf("Send: %v", err)
	}

	from, rcpt, data := server.received()
	if !strings.Contains(from, "octochan@example") {
		t.Errorf("MAIL FROM: %q", from)
	}
	if len(rcpt) != 2 {
		t.Fatalf("получателей дошло %d, отправляли 2: %v", len(rcpt), rcpt)
	}
	if !strings.Contains(rcpt[1], "duty@example") {
		t.Errorf("второй получатель потерян: %q", rcpt[1])
	}

	for _, want := range []string{"Изменений: 2", "multipart/mixed", "=?utf-8?"} {
		if !strings.Contains(data, want) {
			t.Errorf("сервер не получил %q", want)
		}
	}
}

func TestSendRefusesCleartextWhenTLSRequired(t *testing.T) {
	// Сервер без STARTTLS. С требованием шифрования письмо не должно уйти:
	// молча отправить открытым текстом — худший из возможных исходов.
	server := newFakeSMTP(t)
	host, port := server.addr()

	cfg := MailConfig{
		Host: host, Port: port,
		From: "a@example", To: []string{"b@example"},
		StartTLS: true,
	}

	err := Send(cfg, Mail{Subject: "тест", Body: "тело"})
	if err == nil {
		t.Fatal("письмо ушло открытым текстом вопреки требованию шифрования")
	}
	if !errors.Is(err, ErrNoTLS) {
		t.Errorf("ошибка не опознаётся как отсутствие TLS: %v", err)
	}

	if _, _, data := server.received(); data != "" {
		t.Error("тело письма всё-таки было передано серверу")
	}
	if server.sawCommand("MAIL FROM") {
		t.Error("отправитель передан серверу до отказа")
	}
}

func TestSendWithoutTLSRequirementProceeds(t *testing.T) {
	// Тот же сервер без STARTTLS, но без требования шифрования:
	// письмо должно уйти, иначе настройка по умолчанию ломает отправку.
	server := newFakeSMTP(t)
	host, port := server.addr()

	cfg := MailConfig{
		Host: host, Port: port,
		From: "a@example", To: []string{"b@example"},
	}

	if err := Send(cfg, Mail{Subject: "тест", Body: "тело"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, _, data := server.received(); !strings.Contains(data, "тело") {
		t.Error("письмо не дошло")
	}
}

func TestSendReportsServerRefusal(t *testing.T) {
	// Сервер отвергает получателя — ошибка обязана назвать, кого именно,
	// иначе при списке рассылки непонятно, что чинить.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		conn.Write([]byte("220 fake\r\n"))
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			switch verb := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"),
				strings.HasPrefix(verb, "MAIL FROM:"):
				conn.Write([]byte("250 OK\r\n"))
			case strings.HasPrefix(verb, "RCPT TO:"):
				conn.Write([]byte("550 нет такого ящика\r\n"))
			default:
				conn.Write([]byte("221 пока\r\n"))
				return
			}
		}
	}()

	addr := listener.Addr().(*net.TCPAddr)
	cfg := MailConfig{
		Host: "127.0.0.1", Port: addr.Port,
		From: "a@example", To: []string{"нет-такого@example"},
	}

	err = Send(cfg, Mail{Subject: "тест", Body: "тело"})
	if err == nil {
		t.Fatal("отказ сервера не привёл к ошибке")
	}
	if !strings.Contains(err.Error(), "нет-такого@example") {
		t.Errorf("в ошибке не назван отвергнутый получатель: %v", err)
	}
}

func TestSendUnreachableServer(t *testing.T) {
	// Недоступный сервер — это сбой отправки, а не «почта не настроена»:
	// вызывающий различает эти случаи и по-разному на них реагирует.
	cfg := MailConfig{
		Host: "127.0.0.1", Port: 1,
		From: "a@example", To: []string{"b@example"},
	}

	err := Send(cfg, Mail{Subject: "тест", Body: "тело"})
	if err == nil {
		t.Fatal("отправка на закрытый порт прошла успешно")
	}
	if errors.Is(err, ErrMailNotConfigured) {
		t.Error("сбой соединения выдан за отсутствие настройки")
	}
}
