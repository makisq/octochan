package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// config show печатает действующие настройки с источником и не печатает
// секретов: значение токена или пароля скрыто, путь к ключу — нет.
func TestConfigShow(t *testing.T) {
	e := newScriptEnv(t)
	e.write(viper.ConfigFileUsed(), "defaults:\n  http_timeout: 45s\n  api_key: abc123\nexecutors:\n  demo:\n    token: zzz\n    smtp_password: p@ss\n")
	if err := viper.ReadInConfig(); err != nil {
		t.Fatal(err)
	}

	out := e.must("config", "show")
	for _, want := range []string{"defaults.http_timeout", "= 45s  [config.yaml]", "defaults.max_parallel_tasks",
		"= 5  [умолчание]", "defaults.api_key", "= ***  [config.yaml; скрыто]", "executors.demo.token",
		"executors.demo.smtp_password", "tls.key"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"abc123", "zzz", "p@ss"} {
		if strings.Contains(out, secret) {
			t.Fatalf("config show напечатал секрет %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "server.key  [умолчание]") {
		t.Fatalf("путь к ключу TLS скрыт, а это не секрет:\n%s", out)
	}

	out = e.must("config", "show", "--json", "executors")
	var got struct {
		File     string        `json:"file"`
		Settings []configShown `json:"settings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("config show --json: %v\n%s", err, out)
	}
	if len(got.Settings) != 2 || got.Settings[1].Key != "executors.demo.token" || got.Settings[1].Value != hiddenValue ||
		!got.Settings[1].Secret || got.Settings[1].Source != "config" {
		t.Fatalf("config show --json executors: %+v", got.Settings)
	}
	e.fails("раздела nope в настройках нет; разделы: defaults", "config", "show", "nope")
}

func TestSecretSetting(t *testing.T) {
	for key, want := range map[string]bool{
		"defaults.api_key": true, "executors.x.token": true, "a.smtp_password": true, "a.session-cookie": true,
		"a.secret": true, "a.authorization": true, "defaults.token_lifetime": false, "defaults.token_timer_enabled": false,
		"ha.token_file": false, "logs.keep": false, "defaults.smtp_user": false, "a.bypass": false,
	} {
		if got := secretSetting(key, "value"); got != want {
			t.Errorf("%s: %v, ожидалось %v", key, got, want)
		}
	}
	if secretSetting("tls.key", "/etc/ochan/server.key") || secretSetting("a.token", "") {
		t.Error("путь к файлу и пустое значение — не секрет")
	}
}
