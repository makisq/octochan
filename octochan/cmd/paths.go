package cmd

import (
	"strings"

	"ochan/rbac"
	"ochan/workspace"

	"github.com/spf13/cobra"
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
		"Путь — полный. Вынесенный каталог и каждый каталог над ним должны\n" +
		"принадлежать вам или root и быть закрыты для записи группе и остальным:\n" +
		"иначе другой пользователь машины подложил бы в модули свой плагин. Защита\n" +
		"рабочего каталога действует и на вынесенные.",
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
		return nil
	},
}

func init() {
	rootCmd.AddCommand(pathsCmd)
}
