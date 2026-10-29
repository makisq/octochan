package cmd

import (
	"strings"

	"octochan/rbac"

	"github.com/spf13/cobra"
)

var roleProofCmd = &cobra.Command{
	Use:   "proof",
	Short: "Что машина доказывает о том, кто ты и какая у тебя роль",
	Long: "Отвечает на вопрос «что конкретно машина доказывает, что пользователь —\n" +
		"это роль X?»: кто запустил процесс по словам ядра, есть ли системная\n" +
		"политика ролей, которой владеет root, что она положила этому\n" +
		"пользователю и каков потолок роли. И чего это не доказывает.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		line := func(title, text string) { cmd.Printf("%-20s%s\n", title+":", text) }
		id := rbac.CurrentIdentity()
		line("Пользователь ОС", id.String()+" — uid сообщает ядро, имя — база пользователей ОС, не переменные окружения")

		store, path, err := loadRoles()
		if err != nil {
			return err
		}
		role, forced, err := activeRole(store)
		if err == nil {
			role, err = constrain(cmd, store, role, forced, false)
		}
		sys, sysErr := systemPolicy()
		switch {
		case sysErr != nil:
			line("Системная политика", sysErr.Error())
			line("Итог", "ochan не работает: обещанной границы нет, а без неё хуже, чем никак")
			return nil
		case sys == nil:
			line("Системная политика", "нет ("+rbac.SystemPolicyPath+")")
		default:
			line("Системная политика", sys.Path+" — владелец root, писать в файл и в каталоги до корня может только root")
			line("Положено", orNone(sys.Allowed(id)))
		}
		if err != nil {
			line("Роль", err.Error())
			return nil
		}
		source := "из хранилища ролей " + path
		if forced {
			source = "задана окружением " + EnvRole
		}
		line("Роль", role.Name+" — "+source)
		if c := role.Ceiling(); c != nil {
			text := orNone(c.Allow)
			if len(c.Deny) > 0 {
				text += "; запрещено: " + strings.Join(c.Deny, ", ")
			}
			line("Потолок", text+" (системная политика)")
		}

		if sys == nil {
			line("Доказано", "только то, что вы владелец рабочего каталога: кто правит ~/.octochan, тот master")
			line("Не доказано", "ничего о роли: хранилище ролей ваше, и вы можете его переписать")
			return nil
		}
		granted := "; root — что ему положена роль " + role.Name
		if role.Ceiling() != nil {
			granted += " не выше потолка"
		}
		line("Доказано", "ядро — что команду запустил "+id.String()+granted)
		line("Хранилище ролей", path+" — ваше: сужает права роли, но не поднимает их выше потолка")
		line("Не доказано", "что вы не сделаете того же без ochan: свои файлы и свою сеть вам не запретить")
		cmd.Printf("%-20s%s\n", "", "граница держит только то, до чего вы сами не дотягиваетесь (docs/guide/security.md)")
		return nil
	},
}

func init() {
	roleCmd.AddCommand(roleProofCmd)
}
