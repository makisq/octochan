package cmd

import (
	"encoding/json"
	"os"
	"sort"
	"strings"

	"ochan/core/i18n"
	"ochan/core/lang"
	"ochan/core/workspace"
	"ochan/modules/pluginhost"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// config show — какие настройки ochan сейчас действуют и откуда каждая:
// из config.yaml, умолчание ochan или умолчание модуля. Секреты не
// печатаются: значение настройки, похожей на токен или пароль, скрыто.

var (
	configJSON     bool
	configExecutor string
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Настройки ochan: что действует и откуда",
	Long: "Настройки ochan — config.yaml. Файл с пояснениями ochan пишет сам при\n" +
		"первом запуске; модули дописывают в него свои настройки при !!install.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var configShowCmd = &cobra.Command{
	Use:   "show [раздел]",
	Short: "Показать действующие настройки и откуда каждая",
	Long: "Показывает каждую настройку с её значением сейчас и источником:\n" +
		"config.yaml, умолчание ochan или умолчание модуля (modules.<модуль>).\n" +
		"Раздел — начало ключа: defaults, shell, modules.pgstat. Значение\n" +
		"настройки, похожей на секрет (token, password, secret, cookie, key и\n" +
		"подобные, кроме путей к файлам), скрыто: config show можно показать\n" +
		"другому. Токены исполнителей живут не в конфиге — ochan auth.",
	Example: "ochan config show\nochan config show defaults\nochan config show --json modules\n" +
		"ochan config show --executor demo",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if configExecutor != "" {
			if len(args) > 0 {
				return i18n.Errorf("--executor показывает все настройки исполнителя: раздел не нужен")
			}
			return showExecutorConfig(cmd, strings.TrimSpace(configExecutor))
		}
		section := ""
		if len(args) == 1 {
			section = strings.Trim(strings.TrimSpace(args[0]), ".")
		}
		entries := configEntriesNow(section)
		if len(entries) == 0 {
			return i18n.Errorf("раздела %s в настройках нет; разделы: %s", section, strings.Join(configTopSections(), ", "))
		}
		if configJSON {
			data, err := json.MarshalIndent(struct {
				File     string        `json:"file"`
				Settings []configShown `json:"settings"`
			}{viper.ConfigFileUsed(), entries}, "", "  ")
			if err != nil {
				return err
			}
			cmd.Println(string(data))
			return nil
		}
		cmd.Println(i18n.Sprintf("Конфиг: %s", viper.ConfigFileUsed()))
		width := 0
		for _, e := range entries {
			width = max(width, len(e.Key))
		}
		for _, e := range entries {
			cmd.Printf("%-*s = %s  [%s]\n", width, e.Key, e.Text, e.sourceText())
		}
		return nil
	},
}

// configShown — настройка для config show: ключ, значение (скрытое у
// секрета) и источник: config, default или module:<имя>.
type configShown struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Source string `json:"source"`
	Secret bool   `json:"secret,omitempty"`
	// Required — обязательная настройка модуля без значения.
	Required bool   `json:"missing_required,omitempty"`
	Text     string `json:"-"`
}

func (e configShown) sourceText() string {
	text := i18n.T("умолчание")
	switch {
	case e.Source == "config":
		text = "config.yaml"
	case strings.HasPrefix(e.Source, "module:"):
		text = i18n.Sprintf("умолчание модуля %s", strings.TrimPrefix(e.Source, "module:"))
	}
	if e.Secret {
		text += i18n.T("; скрыто")
	}
	if e.Required {
		text += i18n.T("; обязательно, не задано")
	}
	return text
}

// hiddenValue — что config show печатает вместо секрета.
const hiddenValue = "…"

// configEntriesNow — действующие настройки раздела (пусто — все) по
// порядку ключей.
func configEntriesNow(section string) []configShown {
	owners := map[string]string{}
	required := map[string]bool{}
	for _, info := range pluginhost.GetModuleManager().LoadedModules() {
		for _, s := range info.Settings {
			key := moduleSettingKey(info.Name, s.Key)
			owners[key] = info.Name
			required[key] = s.Required
		}
	}
	keys := viper.AllKeys()
	for key := range owners {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []configShown
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] || section != "" && key != section && !strings.HasPrefix(key, section+".") {
			continue
		}
		seen[key] = true
		value := viper.Get(key)
		e := configShown{Key: key, Value: value, Source: "default", Text: settingText(value)}
		switch {
		case viper.InConfig(key):
			e.Source = "config"
		case owners[key] != "":
			e.Source = "module:" + owners[key]
		}
		if e.Text == "" {
			e.Text = `""`
			e.Required = required[key]
		}
		if secretSetting(key, value) {
			e.Secret, e.Value, e.Text = true, hiddenValue, hiddenValue
		}
		out = append(out, e)
	}
	return out
}

// configTopSections — разделы верхнего уровня: подсказка к опечатке.
func configTopSections() []string {
	seen := map[string]bool{}
	var out []string
	for _, key := range viper.AllKeys() {
		top, _, _ := strings.Cut(key, ".")
		if !seen[top] {
			seen[top] = true
			out = append(out, top)
		}
	}
	sort.Strings(out)
	return out
}

// secretSetting — значение настройки скрывается: имя похоже на секрет, а
// значение задано и не путь к файлу (tls.key — путь к ключу, не ключ).
// Словарь и список скрываются целиком, если их имя — секрет.
func secretSetting(key string, value any) bool {
	if !lang.SecretName(key[strings.LastIndex(key, ".")+1:]) {
		return false
	}
	text := strings.TrimSpace(settingText(value))
	return text != "" && !looksLikePath(text)
}

// looksLikePath — текст похож на путь к файлу, а не на секрет.
func looksLikePath(text string) bool {
	for _, prefix := range []string{"/", "~/", "./", "../", "$HOME", "${"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return len(text) > 2 && text[1] == ':' && (text[2] == '\\' || text[2] == '/')
}

func init() {
	configShowCmd.Flags().BoolVar(&configJSON, "json", false, "вывести JSON — для программ")
	configShowCmd.Flags().StringVar(&configExecutor, "executor", "",
		"что исполнитель (или модуль команд) получает в каждом вызове и из какого слоя")
	configCmd.AddCommand(configShowCmd)
	rootCmd.AddCommand(configCmd)
}

// executorShown — настройка исполнителя для config show --executor.
type executorShown struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Key    string `json:"key"`
	Source string `json:"source"`
	Secret bool   `json:"secret,omitempty"`
}

// showExecutorConfig — что исполнитель (или команда модуля) получает в
// каждом вызове: настройки по слоям defaults.* → modules.<модуль> →
// executors.<имя> с ключом, откуда каждая, и есть ли токен.
func showExecutorConfig(cmd *cobra.Command, executor string) error {
	known, runner := false, false
	for _, r := range pluginhost.GetModuleManager().Runners() {
		runner = runner || strings.EqualFold(scenarioExecutorName(r.Kind), executor)
	}
	known = runner
	for _, info := range pluginhost.GetModuleManager().LoadedModules() {
		known = known || info.Name == executor
	}
	if !known {
		return i18n.Errorf("исполнителя или модуля %s нет: ochan modules — что установлено", executor)
	}
	var shown []executorShown
	for _, l := range executorLayers(executor) {
		e := executorShown{Name: l.Name, Value: l.Value, Key: l.Key, Source: "config"}
		if l.Default {
			e.Source = "default"
		}
		if secretSetting(l.Key, l.Value) {
			e.Secret, e.Value = true, hiddenValue
		}
		shown = append(shown, e)
	}
	token, tokenFrom := "", ""
	if fm, err := mustFileManager(); err == nil {
		token = fm.ExecutorToken(executor)
		switch {
		case token == "":
		case strings.TrimSpace(os.Getenv(workspace.EnvExecutorToken(executor))) != "":
			tokenFrom = workspace.EnvExecutorToken(executor)
		default:
			tokenFrom, _ = fm.ExecutorTokenPath(executor)
		}
	}
	if configJSON {
		data, err := json.MarshalIndent(struct {
			Executor  string          `json:"executor"`
			Settings  []executorShown `json:"settings"`
			Token     bool            `json:"token"`
			TokenFrom string          `json:"token_from,omitempty"`
		}{executor, shown, token != "", tokenFrom}, "", "  ")
		if err != nil {
			return err
		}
		cmd.Println(string(data))
		return nil
	}
	if runner {
		cmd.Println(i18n.Sprintf("Исполнитель %s получает в каждом вызове (слои: defaults.* → modules.<модуль> → executors.%s; сильнее — позже):",
			executor, executor))
	} else {
		cmd.Println(i18n.Sprintf("Команды модуля %s получают в каждом вызове (modules.%s):", executor, executor))
	}
	width := 0
	for _, e := range shown {
		width = max(width, len(e.Name))
	}
	for _, e := range shown {
		source := e.Key
		if e.Source == "default" {
			source += i18n.T(" — умолчание")
		}
		cmd.Printf("  %-*s = %s  [%s]\n", width, e.Name, e.Value, source)
	}
	if token == "" {
		cmd.Println(i18n.Sprintf("  токен: не задан — ochan auth --executor %s или %s", executor, workspace.EnvExecutorToken(executor)))
	} else {
		cmd.Println(i18n.Sprintf("  токен: %s  [%s]", hiddenValue, tokenFrom))
	}
	return nil
}
