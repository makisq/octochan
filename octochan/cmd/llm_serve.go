package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ochan/embedded"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Своя модель для других машин: ochan llm serve выставляет модель llm.model
// по протоколу движка v1 на адресе llm.endpoint. Ochan на другой машине
// говорит с ней по этому адресу (llm.endpoint без llm.model).
//
// У сервера нет аутентификации и TLS: он слушает только локальный адрес, а
// не локальный — с --allow-remote и только за прокси с TLS и
// аутентификацией. Тело запроса — не больше 1 МиБ (протокол — около 16 КиБ,
// задача — единицы КиБ); клиент, который не прислал запрос за 60 с,
// отключается.

const (
	serveMaxBody = 1 << 20
	serveTimeout = 60 * time.Second
)

var llmServeRemote bool

// serveAddress — host:port из llm.endpoint; не локальный — отказ без
// --allow-remote.
func serveAddress(endpoint string, remote bool) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Port() == "" {
		return "", fmt.Errorf("llm.endpoint: %q — сервер слушает http://адрес:порт (TLS — у прокси впереди)", endpoint)
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	local := host == "localhost" || ip != nil && ip.IsLoopback()
	if !local && !remote {
		return "", fmt.Errorf("llm.endpoint: %s — не локальный адрес: у сервера модели нет аутентификации и TLS; "+
			"слушать снаружи — --allow-remote и только за прокси с TLS и аутентификацией", host)
	}
	return u.Host, nil
}

// limitBody — тело запроса не больше serveMaxBody; без Content-Length —
// отказ: медленный клиент не держит модель.
func limitBody(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.ContentLength < 0 {
				http.Error(w, `{"error": "нужен Content-Length"}`, http.StatusLengthRequired)
				return
			}
			if r.ContentLength > serveMaxBody {
				http.Error(w, `{"error": "тело запроса больше 1 МиБ"}`, http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, serveMaxBody)
		}
		h.ServeHTTP(w, r)
	})
}

var llmServeCmd = &cobra.Command{
	Use:   "serve [--allow-remote]",
	Short: "Выставить свою модель по протоколу движка v1 — для ochan на других машинах",
	Long: "Загружает свою модель llm.model в процесс и отвечает по протоколу движка v1\n" +
		"на адресе llm.endpoint: /v1/info, /v1/generate, /v1/next, /v1/score. Ochan\n" +
		"на другой машине говорит с ней по этому адресу — llm.endpoint без\n" +
		"llm.model. На этой машине сервер не нужен: команды ochan загружают модель\n" +
		"сами.\n\n" +
		"Аутентификации и TLS у сервера нет: он слушает только локальный адрес;\n" +
		"не локальный — с --allow-remote и только за прокси с TLS и\n" +
		"аутентификацией. Остановить — Ctrl+C.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dir := llmModelDir()
		if dir == "" {
			return errors.New("llm.model: каталог своей модели — без него выставлять нечего")
		}
		addr, err := serveAddress(strings.TrimSpace(viper.GetString("llm.endpoint")), llmServeRemote)
		if err != nil {
			return err
		}
		m, err := loadOwnModel(dir)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("адрес %s: %w", addr, err)
		}
		info := m.Info()
		srv := &http.Server{Handler: limitBody(embedded.Handler(info, m)), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: serveTimeout, ErrorLog: nil}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
		}()
		cmd.Printf("Своя модель: http://%s — протокол v%d, %s\n", ln.Addr(), embedded.Protocol, describeModel(m))
		if len(info.Knowledge) > 0 {
			cmd.Printf("Знания:      @%s\n", strings.Join(info.Knowledge, ", @"))
		}
		cmd.Println("Остановить — Ctrl+C")
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		cmd.Println("Сервер модели остановлен")
		return nil
	},
}

func init() {
	llmServeCmd.Flags().BoolVar(&llmServeRemote, "allow-remote", false, "слушать не локальный адрес — только за прокси с TLS и аутентификацией")
	llmCmd.AddCommand(llmServeCmd)
}
