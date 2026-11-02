package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Сеть: доступен ли порт, что отвечает DNS, жив ли HTTP и когда кончится
// сертификат — диагностика без nc, dig, curl и openssl, и с сервера без
// них. Право — отдельная группа net, а не read: проверять сеть с машины
// ochan — возможность, которую выдают явно (нейросети, которой подсунули
// вредную инструкцию, незачем сканировать внутренние порты). Вызываются и
// из сценария — ochan("net port db7:5432"); недоступное — ошибка, её
// ловит try.

var netTimeout time.Duration

var netCmd = &cobra.Command{
	Use:   "net",
	Short: "Сеть: порт, DNS, HTTP и сертификаты TLS",
	Long: "Доступен ли порт, что отвечает DNS, жив ли HTTP и когда кончится\n" +
		"сертификат — без nc, dig, curl и openssl. Недоступное — ошибка: в\n" +
		"сценарии её ловит try. Право — net (выдаётся явно, в read его нет).",
}

func elapsed(start time.Time) string {
	d := time.Since(start)
	if d < time.Millisecond {
		return "<1 мс"
	}
	return fmt.Sprintf("%d мс", d.Milliseconds())
}

var netPortCmd = &cobra.Command{
	Use:   "port <хост:порт>...",
	Short: "Принимает ли порт соединения TCP",
	Example: "ochan net port db7:5432\n" +
		"ochan net port db7:5432 db8:5432 pgbouncer:6432 --timeout 1s",
	Args:        cobra.MinimumNArgs(1),
	Annotations: map[string]string{pathArgsFrom: "none"},
	RunE: func(cmd *cobra.Command, args []string) error {
		dialer := &net.Dialer{Timeout: netTimeout}
		var closed []string
		for _, target := range args {
			if _, _, err := net.SplitHostPort(target); err != nil {
				return fmt.Errorf("%s: ожидалось хост:порт", target)
			}
			start := time.Now()
			conn, err := dialer.DialContext(cmd.Context(), "tcp", target)
			if err != nil {
				cmd.Printf("%-28s закрыт   %v\n", target, dialReason(err))
				closed = append(closed, target)
				continue
			}
			conn.Close()
			cmd.Printf("%-28s открыт   %s\n", target, elapsed(start))
		}
		if len(closed) > 0 {
			return fmt.Errorf("не отвечает %d из %d: %s", len(closed), len(args), strings.Join(closed, ", "))
		}
		return nil
	},
}

// dialReason — почему не соединилось, без обёрток net.
func dialReason(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err
	}
	return err
}

var netDNS struct {
	kind, server string
}

var netDNSCmd = &cobra.Command{
	Use:   "dns <имя> [--type A|AAAA|CNAME|MX|TXT|NS|SRV|PTR]",
	Short: "Что отвечает DNS",
	Long: "По умолчанию — адреса (A и AAAA). PTR — имя по адресу, SRV — по имени\n" +
		"вида _postgresql._tcp.example.com. --server — спросить этот сервер, а не\n" +
		"системный. Записей нет — ошибка.",
	Example: "ochan net dns db7.example.com\n" +
		"ochan net dns example.com --type MX\n" +
		"ochan net dns 10.0.0.7 --type PTR --server 10.0.0.2:53",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{pathArgsFrom: "none"},
	RunE: func(cmd *cobra.Command, args []string) error {
		r, server := net.DefaultResolver, "системный"
		if netDNS.server != "" {
			server = netDNS.server
			if _, _, err := net.SplitHostPort(server); err != nil {
				server = net.JoinHostPort(server, "53")
			}
			r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: netTimeout}).DialContext(ctx, network, server)
			}}
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), netTimeout)
		defer cancel()
		name, kind := args[0], strings.ToUpper(netDNS.kind)
		var records []string
		var err error
		switch kind {
		case "", "A", "AAAA":
			var addrs []net.IPAddr
			addrs, err = r.LookupIPAddr(ctx, name)
			for _, a := range addrs {
				if kind == "" || (kind == "A") == (a.IP.To4() != nil) {
					records = append(records, a.IP.String())
				}
			}
		case "CNAME":
			var cname string
			cname, err = r.LookupCNAME(ctx, name)
			if cname != "" {
				records = append(records, cname)
			}
		case "MX":
			var mx []*net.MX
			mx, err = r.LookupMX(ctx, name)
			for _, m := range mx {
				records = append(records, fmt.Sprintf("%d %s", m.Pref, m.Host))
			}
		case "TXT":
			records, err = r.LookupTXT(ctx, name)
		case "NS":
			var ns []*net.NS
			ns, err = r.LookupNS(ctx, name)
			for _, n := range ns {
				records = append(records, n.Host)
			}
		case "SRV":
			var srv []*net.SRV
			_, srv, err = r.LookupSRV(ctx, "", "", name)
			for _, s := range srv {
				records = append(records, fmt.Sprintf("%d %d %d %s", s.Priority, s.Weight, s.Port, s.Target))
			}
		case "PTR":
			records, err = r.LookupAddr(ctx, name)
		default:
			return fmt.Errorf("--type %s: A, AAAA, CNAME, MX, TXT, NS, SRV или PTR", netDNS.kind)
		}
		// В тексте ошибки Go называет сервер из настроек системы, даже
		// когда спрашивали --server: причина — своя, сервер — тот, что
		// спрашивали.
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return fmt.Errorf("%s: %s (сервер DNS: %s)", name, dnsErr.Err, server)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if len(records) == 0 {
			return fmt.Errorf("%s: записей %s нет", name, map[bool]string{true: "A и AAAA", false: kind}[kind == ""])
		}
		for _, rec := range records {
			cmd.Println(rec)
		}
		return nil
	},
}

// netCA — доверенные корни: системные и, с --ca, свой центр сертификации
// (внутренний PKI). Проверку сертификата ochan не выключает никогда.
func netCA(path string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if path == "" {
		return pool, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--ca: %w", err)
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("--ca %s: сертификатов PEM нет", path)
	}
	return pool, nil
}

// certLine — до какого дня действует сертификат и сколько осталось.
func certLine(cert *x509.Certificate) string {
	left := int(time.Until(cert.NotAfter).Hours() / 24)
	return fmt.Sprintf("до %s (осталось %d д.)", cert.NotAfter.UTC().Format("2006-01-02"), left)
}

var netHTTP struct {
	method, ca string
	expect     int
	body       bool
	headers    []string
}

// maxNetBody — сколько тела ответа читает net http.
const maxNetBody = 1 << 20

var netHTTPCmd = &cobra.Command{
	Use:   "http <адрес>",
	Short: "Жив ли HTTP: код, время, размер и сертификат",
	Long: "Код ответа, время, размер тела и, у https, срок сертификата. Код 400 и\n" +
		"выше — ошибка; --expect — ровно этот код. --body печатает тело (не больше\n" +
		"1 МиБ) — например, JSON проверки здоровья для сценария. Сертификат\n" +
		"проверяется всегда; свой центр сертификации — --ca.",
	Example: "ochan net http https://grafana.example.com/api/health\n" +
		"ochan net http http://db7:8008/primary --expect 200\n" +
		"ochan net http https://vault.internal/v1/sys/health --ca corp-ca.pem --body",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{pathArgsFrom: "none"},
	RunE: func(cmd *cobra.Command, args []string) error {
		u, err := url.Parse(args[0])
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s: ожидался адрес http:// или https://", args[0])
		}
		pool, err := netCA(netHTTP.ca)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(cmd.Context(), strings.ToUpper(netHTTP.method), u.String(), nil)
		if err != nil {
			return err
		}
		for _, h := range netHTTP.headers {
			key, value, ok := strings.Cut(h, ":")
			if !ok {
				return fmt.Errorf("--header %q: ожидалось имя: значение", h)
			}
			req.Header.Set(strings.TrimSpace(key), strings.TrimSpace(value))
		}
		client := &http.Client{Timeout: netTimeout, Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{RootCAs: pool}}}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("%s: %w", u.Redacted(), err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxNetBody+1))
		took := elapsed(start)
		if err != nil {
			return fmt.Errorf("%s: тело ответа: %w", u.Redacted(), err)
		}
		size := humanBytes(len(data))
		if len(data) > maxNetBody {
			data, size = data[:maxNetBody], "больше 1 МиБ"
		}
		cmd.Printf("%s  %s  %s\n", resp.Status, took, size)
		if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
			cmd.Printf("сертификат %s\n", certLine(resp.TLS.PeerCertificates[0]))
		}
		if netHTTP.body {
			cmd.Println(strings.TrimRight(string(data), "\n"))
		}
		switch {
		case netHTTP.expect != 0 && resp.StatusCode != netHTTP.expect:
			return fmt.Errorf("%s: код %d, ожидался %d", u.Redacted(), resp.StatusCode, netHTTP.expect)
		case netHTTP.expect == 0 && resp.StatusCode >= 400:
			return fmt.Errorf("%s: код %d", u.Redacted(), resp.StatusCode)
		}
		return nil
	},
}

func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d Б", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f КиБ", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f МиБ", float64(n)/(1<<20))
}

var netTLS struct {
	name, ca, warn string
}

var netTLSCmd = &cobra.Command{
	Use:   "tls <хост[:порт]>",
	Short: "Сертификат TLS: кто выдал, кому и сколько ему осталось",
	Long: "Порт по умолчанию — 443. Сертификат проверяется по системным корням и\n" +
		"--ca; не прошёл проверку — сведения о нём всё равно печатаются, а команда\n" +
		"— ошибка. Истёк или осталось меньше --warn (по умолчанию 30d) — ошибка:\n" +
		"так ochan net tls годится для проверки по расписанию.",
	Example: "ochan net tls grafana.example.com\n" +
		"ochan net tls db7:5433 --name db7.internal --ca corp-ca.pem --warn 14d",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{pathArgsFrom: "none"},
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]
		if _, _, err := net.SplitHostPort(target); err != nil {
			target = net.JoinHostPort(target, "443")
		}
		host, _, _ := net.SplitHostPort(target)
		name := netTLS.name
		if name == "" {
			name = host
		}
		warn, err := parseDays(netTLS.warn)
		if err != nil {
			return fmt.Errorf("--warn: %w", err)
		}
		pool, err := netCA(netTLS.ca)
		if err != nil {
			return err
		}
		dial := func(cfg *tls.Config) (*tls.Conn, error) {
			d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: netTimeout}, Config: cfg}
			conn, err := d.DialContext(cmd.Context(), "tcp", target)
			if err != nil {
				return nil, err
			}
			return conn.(*tls.Conn), nil
		}
		conn, verifyErr := dial(&tls.Config{ServerName: name, RootCAs: pool})
		if verifyErr != nil {
			var cert *tls.CertificateVerificationError
			if !errors.As(verifyErr, &cert) {
				return fmt.Errorf("%s: %w", target, dialReason(verifyErr))
			}
			// Сведения нужны и о непроверенном сертификате: чем он плох.
			// Соединение только читает сертификат и сразу закрывается.
			if conn, err = dial(&tls.Config{ServerName: name, InsecureSkipVerify: true}); err != nil {
				return fmt.Errorf("%s: %w", target, dialReason(err))
			}
		}
		state := conn.ConnectionState()
		conn.Close()
		leaf := state.PeerCertificates[0]
		cmd.Printf("кому:      %s\n", leaf.Subject.CommonName)
		cmd.Printf("выдал:     %s\n", leaf.Issuer.CommonName)
		cmd.Printf("действует: с %s %s\n", leaf.NotBefore.UTC().Format("2006-01-02"), certLine(leaf))
		var names []string
		names = append(names, leaf.DNSNames...)
		for _, ip := range leaf.IPAddresses {
			names = append(names, ip.String())
		}
		if len(names) > 0 {
			cmd.Printf("имена:     %s\n", strings.Join(names, ", "))
		}
		cmd.Printf("TLS:       %s\n", tls.VersionName(state.Version))
		switch left := time.Until(leaf.NotAfter); {
		case verifyErr != nil:
			return fmt.Errorf("%s: сертификат не прошёл проверку: %v", target, verifyErr)
		case left <= 0:
			return fmt.Errorf("%s: сертификат истёк %s", target, leaf.NotAfter.UTC().Format("2006-01-02"))
		case left < warn:
			return fmt.Errorf("%s: сертификат истекает через %d д. — меньше --warn %s", target, int(left.Hours()/24), netTLS.warn)
		}
		return nil
	},
}

func init() {
	netCmd.PersistentFlags().DurationVar(&netTimeout, "timeout", 5*time.Second, "сколько ждать ответа")
	netDNSCmd.Flags().StringVar(&netDNS.kind, "type", "", "вид записи: A, AAAA, CNAME, MX, TXT, NS, SRV, PTR")
	netDNSCmd.Flags().StringVar(&netDNS.server, "server", "", "спросить этот сервер DNS (адрес[:порт])")
	f := netHTTPCmd.Flags()
	f.StringVarP(&netHTTP.method, "method", "X", "GET", "метод запроса")
	f.IntVar(&netHTTP.expect, "expect", 0, "ожидаемый код ответа; 0 — любой ниже 400")
	f.BoolVar(&netHTTP.body, "body", false, "напечатать тело ответа (не больше 1 МиБ)")
	f.StringVar(&netHTTP.ca, "ca", "", "свой центр сертификации (PEM) вдобавок к системным")
	f.StringArrayVarP(&netHTTP.headers, "header", "H", nil, "заголовок запроса «Имя: значение»")
	f = netTLSCmd.Flags()
	f.StringVar(&netTLS.name, "name", "", "имя сервера для SNI и проверки; по умолчанию — хост")
	f.StringVar(&netTLS.ca, "ca", "", "свой центр сертификации (PEM) вдобавок к системным")
	f.StringVar(&netTLS.warn, "warn", "30d", "ошибка, если до конца сертификата меньше этого срока")
	netCmd.AddCommand(netPortCmd, netDNSCmd, netHTTPCmd, netTLSCmd)
	rootCmd.AddCommand(netCmd)
}
