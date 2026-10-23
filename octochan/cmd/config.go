package cmd

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"octochan/rlm"
	"octochan/workspace"

	"github.com/spf13/viper"
	"gopkg.in/natefinch/lumberjack.v2"
)

// logFileName должен совпадать у писателя и у команды logs.
// Раньше журнал писался в rlm-cli.log, а команда logs читала octochan.log,
// поэтому она всегда сообщала, что записей ещё нет.
const logFileName = "octochan.log"

// fileManager — общий на процесс, создаётся один раз в initConfig.
var fileManager *workspace.FileManager

func mustFileManager() (*workspace.FileManager, error) {
	if fileManager == nil {
		return nil, fmt.Errorf("рабочие директории не инициализированы")
	}
	return fileManager, nil
}

// configPath определяет, откуда читать конфиг.
// Флаг --config раньше объявлялся, но нигде не использовался.
func configPath(fm *workspace.FileManager) string {
	if cfgFile != "" {
		return cfgFile
	}
	if env := os.Getenv("OCTOCHAN_CONFIG"); env != "" {
		return env
	}
	return filepath.Join(fm.BaseDir(), "config.yaml")
}

func initConfig() error {
	fm, err := workspace.NewFileManager()
	if err != nil {
		return err
	}
	fileManager = fm

	// Все пути живут под ~/.octochan. Прежняя версия держала config.yaml
	// в ~/octochan, а всё остальное в ~/.octochan.
	viper.SetDefault("defaults.api_url", "")
	viper.SetDefault("defaults.scenario_path", fm.ScenariosDir())
	viper.SetDefault("defaults.log_path", fm.LogsDir())
	viper.SetDefault("defaults.auto_check_status", false)
	viper.SetDefault("defaults.max_parallel_tasks", 5)
	viper.SetDefault("defaults.log_max_size", 10)
	viper.SetDefault("defaults.log_max_backups", 5)
	viper.SetDefault("defaults.http_timeout", "30s")
	viper.SetDefault("defaults.module_command_timeout", "10m")
	viper.SetDefault("defaults.plugin_log_level", "warn")
	viper.SetDefault("defaults.allow_unverified_modules", false)
	viper.SetDefault("defaults.token_timer_enabled", true)
	viper.SetDefault("defaults.token_lifetime", "20m")

	// Почта. Пароль сюда не кладётся никогда — он читается из переменной
	// окружения OCTOCHAN_SMTP_PASSWORD, потому что конфиг лежит на диске
	// и попадает в резервные копии.
	viper.SetDefault("defaults.smtp_host", "")
	viper.SetDefault("defaults.smtp_port", 25)
	viper.SetDefault("defaults.smtp_from", "")
	viper.SetDefault("defaults.smtp_to", []string{})
	viper.SetDefault("defaults.smtp_user", "")
	viper.SetDefault("defaults.smtp_starttls", false)
	viper.SetDefault("defaults.notify_always", false)

	// Серверный режим. ssl включает шифрование, tls задаёт его настройки.
	viper.SetDefault("ssl", true)
	viper.SetDefault("tls.min_version", "1.2")
	viper.SetDefault("tls.cert", filepath.Join(fm.BaseDir(), "tls", "server.crt"))
	viper.SetDefault("tls.key", filepath.Join(fm.BaseDir(), "tls", "server.key"))
	viper.SetDefault("tls.ca", "")
	viper.SetDefault("tls.server_name", "")
	viper.SetDefault("server.listen", "127.0.0.1:7443")
	viper.SetDefault("server.dir", "")
	viper.SetDefault("server.session_timeout", "30m")
	viper.SetDefault("server.command_timeout", "10m")

	path := configPath(fm)
	viper.SetConfigFile(path)

	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		// Конфига ещё нет — создаём со значениями по умолчанию.
		if err := viper.WriteConfigAs(path); err != nil {
			return fmt.Errorf("не удалось создать конфиг %s: %w", path, err)
		}
	} else if err := viper.ReadInConfig(); err != nil {
		return fmt.Errorf("не удалось прочитать конфиг %s: %w", path, err)
	}

	// Токен читается отдельно и в viper не попадает, чтобы не уехать
	// в config.yaml при следующем WriteConfig.
	if err := rlm.LoadTokenFrom(fm.TokenFilePath()); err != nil {
		return fmt.Errorf("не удалось прочитать токен: %w", err)
	}

	return nil
}

// initLogger направляет журнал в файл.
//
// Раньше здесь стоял io.MultiWriter(os.Stdout, writer): диагностика уезжала
// в stdout вперемешку с полезным выводом и ломала конвейеры вида
// `ochan print a.conf | ochan diff -`.
func initLogger() error {
	logPath := viper.GetString("defaults.log_path")
	if logPath == "" {
		fm, err := mustFileManager()
		if err != nil {
			return err
		}
		logPath = fm.LogsDir()
	}

	if err := os.MkdirAll(logPath, 0o700); err != nil {
		return fmt.Errorf("не удалось создать директорию логов %s: %w", logPath, err)
	}

	writer := &lumberjack.Logger{
		Filename:   filepath.Join(logPath, logFileName),
		MaxSize:    viper.GetInt("defaults.log_max_size"),
		MaxBackups: viper.GetInt("defaults.log_max_backups"),
		Compress:   true,
	}

	var out io.Writer = writer
	if verbose {
		out = io.MultiWriter(os.Stderr, writer)
	}

	log.SetOutput(out)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	return nil
}
