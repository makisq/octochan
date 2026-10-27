package cmd

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"octochan/i18n"
	"octochan/logs"
	"octochan/workspace"

	"github.com/spf13/viper"
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
	// Адрес API, время запроса и параллельность — общие умолчания для
	// исполнителей модулей; свои у исполнителя — executors.<имя>.
	viper.SetDefault("defaults.api_url", "")
	viper.SetDefault("defaults.scenario_path", fm.ScenariosDir())
	viper.SetDefault("defaults.log_path", fm.LogsDir())
	viper.SetDefault("defaults.auto_check_status", false)
	viper.SetDefault("defaults.max_parallel_tasks", 5)
	viper.SetDefault("defaults.http_timeout", "30s")
	viper.SetDefault("defaults.module_command_timeout", "10m")
	viper.SetDefault("defaults.plugin_log_level", "warn")
	viper.SetDefault("defaults.allow_unverified_modules", false)
	// Напоминание о смене токена исполнителя — по желанию: ochan timer on.
	viper.SetDefault("defaults.token_timer_enabled", false)
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

	// Язык справки и сообщений: ru, en (eng) или язык из модуля.
	viper.SetDefault("lang", i18n.Source)

	// Журнал ochan и ранжирование журналов по уровням важности.
	viper.SetDefault("logs.max_size", "100MB")
	viper.SetDefault("logs.prefix", logs.DefaultPrefix)
	viper.SetDefault("logs.keep", 10)
	viper.SetDefault("logs.levels", logs.DefaultLevels)
	viper.SetDefault("logs.default_level", "info")
	viper.SetDefault("logs.min_level", "warning")
	viper.SetDefault("logs.rules", logs.DefaultRules)

	// Песочница ОС для сценариев с --sandbox: strict — отказываться
	// выполнять, если ядро не даёт полной изоляции.
	viper.SetDefault("sandbox.strict", false)

	// Оболочка: сколько команд помнить между запусками и показывать ли
	// логотип при входе.
	viper.SetDefault("shell.history_size", 1000)
	viper.SetDefault("shell.logo", true)

	// Как оператор ставит задачу модели: formal — только формальным
	// промптом, natural — на естественном языке.
	viper.SetDefault("fp.mode", "formal")

	// Кольца формального промпта: порог well_True для плана и для заявки,
	// условный счёт правила без журнала, показывать ли оценки и сколько
	// отказов подряд по одному промпту допустимо до оператора.
	viper.SetDefault("fp.rings.enabled", true)
	viper.SetDefault("fp.rings.well_true.plan", 0.5)
	viper.SetDefault("fp.rings.well_true.apply", 0.6)
	viper.SetDefault("fp.rings.prior", "8/8")
	viper.SetDefault("fp.rings.show_scores", false)
	viper.SetDefault("fp.rings.budget.plan", 3)

	// Пределы частоты ролей сверх тех, что задаёт role limit: в той же
	// записи. Модель не строит больше 60 планов в час; [] снимает предел.
	viper.SetDefault("roles.llm.limits", []string{"plan=60/1h"})

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

	maxSize, err := logs.ParseSize(viper.GetString("logs.max_size"))
	if err != nil {
		return fmt.Errorf("logs.max_size: %w", err)
	}
	prefix := viper.GetString("logs.prefix")
	if err := logs.CheckPrefix(prefix); err != nil {
		return err
	}
	writer := &logs.Rotator{
		Dir:     logPath,
		Name:    logFileName,
		MaxSize: maxSize,
		Prefix:  prefix,
		Keep:    viper.GetInt("logs.keep"),
	}

	var out io.Writer = writer
	if verbose {
		out = io.MultiWriter(os.Stderr, writer)
	}

	log.SetOutput(out)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	return nil
}
