package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"octochan/lang"
	"octochan/rbac"

	"github.com/spf13/cobra"
)

var (
	roleDeny       []string
	roleScopeClear bool
	roleLimitClear bool
)

var denyTemp tempFlags

var roleDenyCmd = &cobra.Command{
	Use:   "deny <роль> <право>... [--for <срок> | --until <момент>]",
	Short: "Запретить роли права: запрет перекрывает разрешение",
	Long: "Запрет сильнее права, в том числе all: роль с all и запретом serve\n" +
		"может всё, кроме serve. Команды, без которых не узнать свою роль и не\n" +
		"вернуться (role, set role, back role, help), не запрещаются.\n\n" +
		"С --for или --until запрет действует до срока — заморозка на время\n" +
		"инцидента или релиза.",
	Args:    cobra.MinimumNArgs(2),
	Example: "ochan role deny ops serve tls\nochan role deny dan role\nochan role deny dan apply --for 4h",
	RunE: func(cmd *cobra.Command, args []string) error {
		now := rbac.Now()
		until, temporary, err := denyTemp.until(now)
		if err != nil {
			return err
		}
		return mutateRoles(func(store *rbac.Store) error {
			if temporary {
				perms := splitPerms(args[1:])
				if err := store.DenyFor(args[0], perms, until, moduleCommands()); err != nil {
					return err
				}
				cmd.Printf("Роль %s: запрещено %s до %s\n", store.Find(args[0]).Name, strings.Join(perms, ", "),
					formatUntil(until, now))
				return nil
			}
			if err := store.DenyPerms(args[0], splitPerms(args[1:]), moduleCommands()); err != nil {
				return err
			}
			role := store.Find(args[0])
			cmd.Printf("Роль %s: запрещено %s\n", role.Name, strings.Join(role.Deny, ", "))
			return nil
		})
	},
}

var roleUndenyCmd = &cobra.Command{
	Use:   "undeny <роль> <право>...",
	Short: "Снять с роли запреты",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mutateRoles(func(store *rbac.Store) error {
			if err := store.Undeny(args[0], splitPerms(args[1:])); err != nil {
				return err
			}
			role := store.Find(args[0])
			cmd.Printf("Роль %s: запрещено %s\n", role.Name, noneOr(role.Deny))
			return nil
		})
	},
}

var roleScopeCmd = &cobra.Command{
	Use:   "scope <роль> <каталог>... | scope <роль> --clear",
	Short: "Ограничить роль каталогами",
	Long: "Роль с областью работает только внутри своих каталогов: текущий\n" +
		"каталог, пути в аргументах и флагах, песочница сценариев, файлы заявок,\n" +
		"которые она согласует или выполняет, и файлы отката — всё должно лежать\n" +
		"в них. Сценарии такой роли всегда идут в песочнице (без --sandbox — с\n" +
		"текущим каталогом), eval ей недоступен.\n\n" +
		"На сервере роль без своей области и так ограничена каталогом сервера\n" +
		"(server.dir). Рабочий каталог ochan закрыт для всех ролей, кроме master.",
	Args:    cobra.MinimumNArgs(1),
	Example: "ochan role scope dan /srv/configs/team-a\nochan role scope dan --clear",
	RunE: func(cmd *cobra.Command, args []string) error {
		var dirs []string
		switch {
		case roleScopeClear && len(args) > 1:
			return errors.New("--clear снимает область: каталоги с ним не указываются")
		case !roleScopeClear && len(args) < 2:
			return errors.New("укажите каталоги области или --clear")
		}
		for _, dir := range args[1:] {
			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			real, err := lang.RealPath(abs)
			if err != nil {
				return err
			}
			if info, err := os.Stat(real); err != nil || !info.IsDir() {
				return fmt.Errorf("%s — не каталог", dir)
			}
			dirs = append(dirs, real)
		}
		return mutateRoles(func(store *rbac.Store) error {
			if err := store.SetScope(args[0], dirs); err != nil {
				return err
			}
			role := store.Find(args[0])
			cmd.Printf("Роль %s: область %s\n", role.Name, noneOr(role.Paths))
			return nil
		})
	},
}

var roleLimitCmd = &cobra.Command{
	Use:   "limit <роль> <право>=<число>/<окно>... | limit <роль> --clear",
	Short: "Ограничить частоту команд роли",
	Long: "Предел — сколько раз за окно роль может выполнить команды права:\n" +
		"apply=10/1h — не больше десяти выполнений в час. Окно — от 1m до 31d.\n" +
		"Считается по журналу аудита, по всем сессиям роли вместе; отказ тоже\n" +
		"пишется в журнал. Предел того же права заменяет прежний.",
	Args:    cobra.MinimumNArgs(1),
	Example: "ochan role limit llm request=30/1h\nochan role limit dan apply=10/1h approve=20/1d\nochan role limit dan --clear",
	RunE: func(cmd *cobra.Command, args []string) error {
		switch {
		case roleLimitClear && len(args) > 1:
			return errors.New("--clear снимает пределы: пределы с ним не указываются")
		case !roleLimitClear && len(args) < 2:
			return errors.New("укажите пределы или --clear")
		}
		var limits []rbac.Limit
		for _, arg := range args[1:] {
			limit, err := rbac.ParseLimit(arg)
			if err != nil {
				return err
			}
			limits = append(limits, limit)
		}
		return mutateRoles(func(store *rbac.Store) error {
			if err := store.SetLimits(args[0], limits, moduleCommands()); err != nil {
				return err
			}
			role := store.Find(args[0])
			cmd.Printf("Роль %s: пределы %s\n", role.Name, noneOr(limitTexts(role.Limits)))
			return nil
		})
	},
}

var roleShowCmd = &cobra.Command{
	Use:   "show [роль]",
	Short: "Показать роль целиком: права, запреты, область, пределы",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, _, err := loadRoles()
		if err != nil {
			return err
		}
		role, _, err := activeRole(store)
		if err != nil {
			return err
		}
		if len(args) == 1 {
			if role = store.Find(args[0]); role == nil {
				return fmt.Errorf("роли %s нет", args[0])
			}
		}
		auth := []string{}
		if role.Login != nil {
			auth = append(auth, "логин")
		}
		if role.Password != nil {
			auth = append(auth, "пароль")
		}
		cmd.Printf("Роль:      %s\n", role.Name)
		cmd.Printf("Права:     %s\n", noneOr(role.Perms))
		cmd.Printf("Запреты:   %s\n", noneOr(role.Deny))
		cmd.Printf("Область:   %s\n", noneOr(role.Paths))
		limits := limitTexts(role.Limits)
		_, fromConfig, err := effectiveLimits(role)
		if err != nil {
			return err
		}
		for _, text := range limitTexts(fromConfig) {
			limits = append(limits, text+" (config.yaml)")
		}
		cmd.Printf("Пределы:   %s\n", noneOr(limits))
		now := rbac.Now()
		if temp := role.ActiveTemp(); len(temp) > 0 {
			cmd.Printf("На время:  %s\n", strings.Join(tempTexts(temp, now), "; "))
		}
		if temp := role.ActiveTempDeny(); len(temp) > 0 {
			cmd.Printf("Запреты на время: %s\n", strings.Join(tempTexts(temp, now), "; "))
		}
		cmd.Printf("Вход:      %s\n", noneOr(auth))
		return nil
	},
}

func limitTexts(limits []rbac.Limit) []string {
	texts := make([]string, len(limits))
	for i, limit := range limits {
		texts[i] = limit.String()
	}
	return texts
}

// noneOr — список через запятую или «нет».
func noneOr(values []string) string {
	if len(values) == 0 {
		return "нет"
	}
	return strings.Join(values, ", ")
}

func init() {
	denyTemp.add(roleDenyCmd, "запретить")
	roleScopeCmd.Flags().BoolVar(&roleScopeClear, "clear", false, "снять область")
	roleLimitCmd.Flags().BoolVar(&roleLimitClear, "clear", false, "снять пределы")
	roleCreateCmd.Flags().StringSliceVar(&roleDeny, "deny", nil, "запреты роли через запятую")
	roleCmd.AddCommand(roleDenyCmd, roleUndenyCmd, roleScopeCmd, roleLimitCmd, roleShowCmd)
}
