package chat

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// transportWithCA — транспорт HTTP, который доверяет системным корневым
// сертификатам и ещё тем, что в файле ca (PEM). Проверка сертификата не
// выключается: свой центр сертификации добавляют, а не обходят.
func transportWithCA(ca string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if ca == "" {
		return transport, nil
	}
	pem, err := os.ReadFile(ca)
	if err != nil {
		return nil, fmt.Errorf("llm.chat.ca: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("llm.chat.ca: в %s нет сертификатов PEM", ca)
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return transport, nil
}

// httpClient — клиент HTTP поставщика с пределом времени запроса.
func httpClient(o Options) (*http.Client, error) {
	transport, err := transportWithCA(o.CA)
	if err != nil {
		return nil, err
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// postJSON — запрос JSON и разбор ответа; who — чьё это API, для ошибок.
func postJSON(ctx context.Context, client *http.Client, who, url string, headers map[string]string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doJSON(client, who, req, out)
}

func doJSON(client *http.Client, who string, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("запрос к %s: %w", who, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("ответ %s: %w", who, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text := strings.TrimSpace(string(raw))
		if len(text) > 600 {
			text = text[:600] + "…"
		}
		switch {
		case resp.StatusCode == 401:
			return fmt.Errorf("%s не принял ключ (401): ochan auth --executor llm — %s", who, text)
		case resp.StatusCode == 404:
			return fmt.Errorf("%s не знает такой модели или адреса (404) — проверьте llm.chat.model и llm.chat.url: %s", who, text)
		case resp.StatusCode == 429:
			return fmt.Errorf("предел запросов %s исчерпан (429) — повторите позже: %s", who, text)
		case resp.StatusCode >= 500:
			return fmt.Errorf("сбой на стороне %s (%d) — повторите позже: %s", who, resp.StatusCode, text)
		}
		return fmt.Errorf("запрос к %s отклонён (%d): %s", who, resp.StatusCode, text)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ответ %s — не JSON: %w", who, err)
	}
	return nil
}
