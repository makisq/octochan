package cmd

import (
	"strings"

	"ochan/rbac"
	"ochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Пути ochan — где что лежит и чем каждый путь меняется.
//
//	ochan.home рядом с программой         # рабочий каталог целиком (ochan setup)
//	OCTOCHAN_HOME=/srv/ochan              # то же окружением: оно сильнее
//	OCTOCHAN_MODULES=/opt/ochan/modules   # каталог узла — отдельно
//
//	paths:                                # то же в config.yaml
//	  modules: /opt/ochan/modules
//	  logs: /var/log/ochan
//
// Рабочий каталог — одно целое: роли, журнал аудита, заявки, история,
// решения, политики, алиасы, инциденты, почта и токены переносятся только
// вместе с ним — его зеркалит резервный узел пары и закрывает защита
// рабочего каталога. Отдельно выносятся каталоги узла, которые в пару не
// копируются. Системную политику ролей задаёт сборка: её пишет root, и
// переменная окружения пользователя её не переносит.

// dirNames — выносные каталоги по-русски.
var dirNames = map[workspace.Dir]string{
	workspace.DirModules:       "Модули",
	workspace.DirLogs:          "Журналы",
	workspace.DirReports:       "Отчёты",
	workspace.DirRegistryCache: "Кеш реестров",
}

var pathsCmd = &cobra.Command{
	Use:   "paths",
	Short: "Где лежит то, чем пользуется ochan, и чем меняется каждый путь",
	Long: "Рабочий каталог (по умолчанию ~/.octochan) переносится целиком: в нём\n" +
		"роли, журнал аудита, заявки, история, решения, политики, алиасы,\n" +
		"инциденты, почта и токены. Это одно целое: его зеркалит резервный узел\n" +
		"пары и закрывает от сценариев и ролей защита рабочего каталога. Где он —\n" +
		"говорит указатель ochan.home рядом с программой (его пишет ochan setup;\n" +
		"путь в нём может быть от папки программы) или переменная OCTOCHAN_HOME —\n" +
		"она сильнее указателя.\n\n" +
		"Каталоги узла выносятся отдельно — переменной окружения или настройкой\n" +
		"paths.* в config.yaml (окружение сильнее): модули (OCTOCHAN_MODULES,\n" +
		"paths.modules), журналы (OCTOCHAN_LOGS, paths.logs), отчёты\n" +
		"(OCTOCHAN_REPORTS, paths.reports), кеш реестров (OCTOCHAN_REGISTRY_CACHE,\n" +
		"paths.registry_cache). Модули выносят туда, где политика хоста разрешает\n" +
		"запуск: в домашнем каталоге с noexec плагин не запускается (operation not\n" +
		"permitted), и перенос решает это, не обходя политику.\n\n" +
		"Путь — полный. Рабочему (не по умолчанию) и вынесенному каталогу и\n" +
		"каждому каталогу над ними надёжнее принадлежать вам или root и быть\n" +
		"закрытыми для записи группе и остальным: иначе другой пользователь\n" +
		"машины подложил бы в модули свой плагин. Открытый каталог — не отказ:\n" +
		"закрыть общую папку можно не всегда. ochan предупреждает о нём при\n" +
		"установке и при запуске, а эта команда показывает его всегда;\n" +
		"предупреждение при запуске выключает paths.warn_open: false в\n" +
		"config.yaml (или ответ ochan setup). Защита рабочего каталога действует\n" +
		"и на вынесенные.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		fm, err := mustFileManager()
		if err != nil {
			return err
		}
		row := func(name, path, source, change string) {
			if source == workspace.SourceDefault && change != "" {
				source += "; меняется: " + change
			}
			cmd.Printf("%-20s %s\n%-20s   %s\n", name, path, "", source)
		}
		home := fm.Home()
		row("Рабочий каталог", home.Path, home.Source, workspace.EnvHome+", "+workspace.HomeFile+" рядом с программой (ochan setup)")
		configSource := workspace.SourceDefault
		switch {
		case cfgFile != "":
			configSource = "флаг --config"
		case configPath(fm) != defaultConfigPath(fm):
			configSource = "окружение OCTOCHAN_CONFIG"
		}
		row("Конфиг", configPath(fm), configSource, "--config, OCTOCHAN_CONFIG")
		for _, dir := range fm.Relocatable() {
			row(dirNames[dir.Dir], dir.Path, dir.Source, dir.Dir.Env()+", paths."+string(dir.Dir))
		}
		row("Системная политика", rbac.SystemPolicyPath, "задаётся при сборке (make install SYSTEM_ROLES=…): её пишет root", "")
		cmd.Println("\nТолько вместе с рабочим каталогом: " + strings.Join([]string{"роли", "журнал аудита", "заявки", "история",
			"решения", "политики", "алиасы", "сценарии", "инциденты", "почта", "токены"}, ", "))
		// Открытые каталоги видны здесь всегда — и когда предупреждение при
		// запуске выключено.
		if notes := fm.OpenDirs(); len(notes) > 0 {
			cmd.Println("\nОткрыты для чужой записи:")
			for _, note := range notes {
				cmd.Println("  " + note)
			}
			if viper.GetBool(pathsWarnOpen) {
				cmd.Printf("  ochan предупреждает об этом при запуске; выключить — %s: false в config.yaml\n", pathsWarnOpen)
			} else {
				cmd.Printf("  предупреждение при запуске выключено (%s: false)\n", pathsWarnOpen)
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(pathsCmd)
}
