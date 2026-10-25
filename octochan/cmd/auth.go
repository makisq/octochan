package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"octochan/rlm"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/term"
)

var authCmd = &cobra.Command{
	Use:   "auth [token]",
	Short: "Установить токен для доступа к RLM API",
	Args:  cobra.MaximumNArgs(1),
	Long: "Токен сохраняется в ~/.octochan/.env с правами 0600.\n" +
		"Альтернатива — переменная окружения " + rlm.EnvTokenVar + ".",
	RunE: func(cmd *cobra.Command, args []string) error {
		fm, err := mustFileManager()
		if err != nil {
			return err
		}

		var token string
		if len(args) == 1 {
			token = strings.TrimSpace(args[0])
		} else {
			token, err = promptForToken(cmd)
			if err != nil {
				return err
			}
		}

		if token == "" {
			return fmt.Errorf("пустой токен")
		}

		if err := rlm.SaveTokenTo(fm.TokenFilePath(), token); err != nil {
			return err
		}

		cmd.Printf("Токен сохранён: %s\n", fm.TokenFilePath())
		// В оболочке напоминание отсчитывается от нового токена.
		restartTokenReminder()
		return nil
	},
}

// promptForToken читает токен без эха.
// Прежняя версия использовала bufio.Scanner, из-за чего токен оставался
// на экране и в истории терминала.
func promptForToken(cmd *cobra.Command) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		cmd.Print("Введите токен: ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return "", fmt.Errorf("не удалось прочитать токен: %w", scanner.Err())
		}
		return strings.TrimSpace(scanner.Text()), nil
	}

	cmd.Print("Введите токен (ввод скрыт): ")
	raw, err := term.ReadPassword(fd)
	cmd.Println()
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать токен: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

var timerCmd = &cobra.Command{
	Use:   "timer [on|off]",
	Short: "Управление напоминанием об истечении токена",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			status := "выключено"
			if viper.GetBool("defaults.token_timer_enabled") {
				status = "включено"
			}
			cmd.Printf("Напоминание о смене токена: %s (время жизни %s)\n",
				status, tokenLifetime())
			if _, ok := tokenSavedAt(); !ok {
				cmd.Println("Токен командой auth не сохранён — напоминать не о чем")
			}
			return nil
		}

		switch args[0] {
		case "on":
			viper.Set("defaults.token_timer_enabled", true)
			cmd.Println("Напоминание о смене токена включено")
		case "off":
			viper.Set("defaults.token_timer_enabled", false)
			cmd.Println("Напоминание о смене токена выключено")
		default:
			return fmt.Errorf("используйте: timer on или timer off")
		}

		// WriteConfig теперь безопасен: токен в viper не попадает,
		// он живёт в отдельном файле с правами 0600.
		if err := viper.WriteConfig(); err != nil {
			return fmt.Errorf("не удалось сохранить настройки: %w", err)
		}
		restartTokenReminder()
		return nil
	},
}

func tokenLifetime() time.Duration {
	if d := viper.GetDuration("defaults.token_lifetime"); d > 0 {
		return d
	}
	return 20 * time.Minute
}

// Напоминание работает только в интерактивной оболочке: в одноразовом
// вызове CLI процесс завершается раньше, чем таймер успевает сработать.
//
// Отсчёт идёт от того, когда токен сохранили командой auth, а не от входа в
// оболочку: иначе ochan напоминал бы «смените токен» тому, кто его не
// вводил и RLM не пользуется. Токен из окружения не напоминается вовсе —
// когда его выдали, ochan не знает.
var (
	tokenTimerMu   sync.Mutex
	tokenTimerStop chan struct{}
	// reminderOut — куда напоминать; задан только в оболочке.
	reminderOut func(string)
)

// tokenSavedAt — когда токен сохранили командой auth; false — не сохраняли.
func tokenSavedAt() (time.Time, bool) {
	fm, err := mustFileManager()
	if err != nil {
		return time.Time{}, false
	}
	info, err := os.Stat(fm.TokenFilePath())
	if err != nil || info.Size() == 0 {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func startTokenReminder(out func(string)) {
	tokenTimerMu.Lock()
	reminderOut = out
	tokenTimerMu.Unlock()
	restartTokenReminder()
}

// restartTokenReminder заводит напоминание заново: после auth или timer on.
func restartTokenReminder() {
	tokenTimerMu.Lock()
	defer tokenTimerMu.Unlock()

	if tokenTimerStop != nil {
		close(tokenTimerStop)
		tokenTimerStop = nil
	}
	out := reminderOut
	if out == nil || !viper.GetBool("defaults.token_timer_enabled") {
		return
	}
	saved, ok := tokenSavedAt()
	if !ok {
		return
	}

	lifetime := tokenLifetime()
	warnAt := saved.Add(lifetime - 5*time.Minute)
	if lifetime <= 5*time.Minute {
		warnAt = saved.Add(lifetime / 2)
	}
	expireAt := saved.Add(lifetime)

	stop := make(chan struct{})
	tokenTimerStop = stop
	go func() {
		now := time.Now()
		if now.Before(warnAt) {
			warn := time.NewTimer(warnAt.Sub(now))
			defer warn.Stop()
			select {
			case <-warn.C:
				out("Внимание: токен станет недействителен примерно через " +
					time.Until(expireAt).Round(time.Minute).String() + ". Обновите его командой 'auth'.")
			case <-stop:
				return
			}
		}
		if wait := time.Until(expireAt); wait > 0 {
			expire := time.NewTimer(wait)
			defer expire.Stop()
			select {
			case <-expire.C:
			case <-stop:
				return
			}
		}
		out("Внимание: время жизни токена истекло. Обновите его командой 'auth'.")
	}()
}

func stopTokenReminder() {
	tokenTimerMu.Lock()
	defer tokenTimerMu.Unlock()

	reminderOut = nil
	if tokenTimerStop != nil {
		close(tokenTimerStop)
		tokenTimerStop = nil
	}
}
