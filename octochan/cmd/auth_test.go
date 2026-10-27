package cmd

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// Напоминание о токене молчит, пока токен не сохранён командой auth, и
// отсчитывает время жизни от сохранения, а не от входа в оболочку.
func TestTokenReminderNeedsSavedToken(t *testing.T) {
	e := newScriptEnv(t)
	viper.Set("defaults.token_timer_enabled", true)
	viper.Set("defaults.token_lifetime", "20m")
	t.Cleanup(func() {
		stopTokenReminder()
		viper.Set("defaults.token_timer_enabled", false)
	})

	messages := make(chan string, 4)
	out := func(msg string) { messages <- msg }

	startTokenReminder(out)
	select {
	case msg := <-messages:
		t.Fatalf("без токена напоминание прозвучало: %s", msg)
	case <-time.After(200 * time.Millisecond):
	}
	if !strings.Contains(e.must("timer"), "напоминать не о чем") {
		t.Fatal("timer не говорит, что токена нет")
	}

	// Токен сохранили час назад — срок вышел.
	e.must("auth", "--executor", "rlm", "token-1")
	fm, _ := mustFileManager()
	path, err := fm.ExecutorTokenPath("rlm")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	restartTokenReminder()
	select {
	case msg := <-messages:
		if !strings.Contains(msg, "истекло") || !strings.Contains(msg, "ochan auth --executor rlm") {
			t.Fatalf("напоминание: %s", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("о вышедшем сроке токена не напомнили")
	}

	// Свежий токен — до предупреждения ещё пятнадцать минут.
	e.must("auth", "--executor", "rlm", "token-2")
	select {
	case msg := <-messages:
		t.Fatalf("о свежем токене напомнили сразу: %s", msg)
	case <-time.After(200 * time.Millisecond):
	}

	// Выключенное напоминание молчит и с вышедшим сроком.
	viper.Set("defaults.token_timer_enabled", false)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	restartTokenReminder()
	select {
	case msg := <-messages:
		t.Fatalf("выключенное напоминание прозвучало: %s", msg)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestTokenReminderOffByDefault(t *testing.T) {
	newScriptEnv(t)
	if viper.GetBool("defaults.token_timer_enabled") {
		t.Fatal("напоминание о токене включено по умолчанию")
	}
}
