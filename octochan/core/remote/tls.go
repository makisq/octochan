// Package remote — серверный режим octochan: общий оркестратор, к которому
// подключаются по сети в одной из его ролей.
//
// Сервер держит роли у себя: логин и пароль проверяет он, и клиент не
// может их обойти, потому что хранилища ролей у него нет. Команды клиента
// выполняются на сервере, в роли сессии. Внутри сессии работают set role
// и back role — со своей историей у каждой сессии.
package remote

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Settings — параметры соединения: ssl включает шифрование, tls задаёт
// его настройки. SSL — старое имя TLS; SSL 3.0 взломан, и Go его не
// поддерживает, поэтому «SSL-подключение» здесь — это TLS.
type Settings struct {
	// SSL — шифровать соединение.
	SSL bool
	// MinVersion — минимальная версия TLS: 1.2 или 1.3.
	MinVersion string
	// Cert и Key — сертификат и ключ сервера.
	Cert string
	Key  string
	// CA — сертификат, которым клиент проверяет сервер. Пусто — системные
	// корневые сертификаты.
	CA string
	// ServerName — имя сервера для проверки сертификата; пусто — из адреса.
	ServerName string
}

func (s Settings) minVersion() (uint16, error) {
	switch strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s.MinVersion)), "tls") {
	case "", "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("tls.min_version: %q, допустимо 1.2 или 1.3 (SSL 3.0, TLS 1.0 и 1.1 небезопасны)", s.MinVersion)
	}
}

// ServerTLS — настройки TLS сервера.
func ServerTLS(s Settings) (*tls.Config, error) {
	version, err := s.minVersion()
	if err != nil {
		return nil, err
	}
	if s.Cert == "" || s.Key == "" {
		return nil, errors.New("не заданы tls.cert и tls.key; создать: ochan tls init")
	}
	pair, err := tls.LoadX509KeyPair(s.Cert, s.Key)
	if err != nil {
		return nil, fmt.Errorf("сертификат сервера: %w; создать: ochan tls init", err)
	}
	return &tls.Config{MinVersion: version, Certificates: []tls.Certificate{pair}}, nil
}

// ClientTLS — настройки TLS клиента. Проверку сертификата отключить
// нельзя: без неё пароль ушёл бы любому, кто встал посередине.
func ClientTLS(s Settings, host string) (*tls.Config, error) {
	version, err := s.minVersion()
	if err != nil {
		return nil, err
	}
	config := &tls.Config{MinVersion: version, ServerName: host}
	if s.ServerName != "" {
		config.ServerName = s.ServerName
	}
	if s.CA != "" {
		data, err := os.ReadFile(s.CA)
		if err != nil {
			return nil, fmt.Errorf("tls.ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("tls.ca: в %s нет сертификатов PEM", s.CA)
		}
		config.RootCAs = pool
	}
	return config, nil
}

// GenerateCert создаёт самоподписанный сертификат сервера ECDSA P-256 и
// возвращает его отпечаток SHA-256. Клиенту этот же сертификат указывается
// как tls.ca.
func GenerateCert(certPath, keyPath string, hosts []string, validity time.Duration) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("не удалось создать ключ: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", fmt.Errorf("не удалось создать серийный номер: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "octochan", Organization: []string{"octochan"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if host != "" {
			template.DNSNames = append(template.DNSNames, host)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", fmt.Errorf("не удалось подписать сертификат: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("не удалось сохранить ключ: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", fmt.Errorf("не удалось записать ключ: %w", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", fmt.Errorf("не удалось записать сертификат: %w", err)
	}
	return Fingerprint(der), nil
}

// Fingerprint — отпечаток сертификата SHA-256 в виде AB:CD:….
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	hexed := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(hexed)/2)
	for i := 0; i < len(hexed); i += 2 {
		parts = append(parts, hexed[i:i+2])
	}
	return strings.Join(parts, ":")
}

// isLoopback сообщает, что адрес — только эта машина.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
