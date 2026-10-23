package cmd

import (
	"strings"

	"octochan/audit"
	"octochan/i18n"

	"github.com/spf13/cobra"
)

var (
	auditLimit  int
	auditRole   string
	auditDenied bool
	auditHead   string
)

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Журнал аудита: кто, что и когда пытался выполнить",
	Long: "Каждая команда, вход на сервер и смена роли пишутся в журнал\n" +
		"~/.octochan/audit.log — и разрешённые, и отвергнутые, с причиной отказа.\n" +
		"Записи связаны цепочкой хешей: правку, вставку или удаление записи\n" +
		"находит ochan audit verify.",
	Args: cobra.NoArgs,
	Example: "ochan audit\n" +
		"ochan audit --role dan --denied\n" +
		"ochan audit verify --head 3f9a1c0b2e7d",
	RunE: func(cmd *cobra.Command, _ []string) error {
		journal, err := auditJournal()
		if err != nil {
			return err
		}
		records, err := journal.Last(auditLimit, func(rec audit.Record) bool {
			if auditRole != "" && !strings.EqualFold(rec.Role, auditRole) {
				return false
			}
			return !auditDenied || !rec.Allowed
		})
		if err != nil {
			return err
		}
		if len(records) == 0 {
			cmd.Println(i18n.T("Записей нет"))
			return nil
		}
		for _, rec := range records {
			mark := "+"
			if !rec.Allowed {
				mark = "-"
			}
			what := strings.TrimSpace(strings.ReplaceAll(rec.Command, ".", " ") + " " + strings.Join(rec.Args, " "))
			if rec.Event != audit.EventCommand {
				what = rec.Event + ": " + what
			}
			where := rec.User
			if rec.Remote != "" {
				where = rec.Remote
			}
			cmd.Printf("%s %s  %-10s %-16s %s\n", mark, rec.Time.Local().Format("2006-01-02 15:04:05"),
				rec.Role, where, what)
			if rec.Reason != "" {
				cmd.Printf("    %s\n", rec.Reason)
			}
		}
		cmd.Println(i18n.T("\n+ разрешено, - отказано"))
		return nil
	},
}

var auditVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Проверить цепочку хешей журнала аудита",
	Long: "Проверяет каждую запись: её хеш, ссылку на предыдущую и номер. Правка,\n" +
		"вставка или удаление записи рвут цепочку — проверка назовёт строку.\n\n" +
		"Обрезанный хвост цепочку не рвёт. Поэтому хеш последней записи стоит\n" +
		"время от времени сохранять вне этой машины: --head с ним проверит, что\n" +
		"эта запись в журнале есть.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		journal, err := auditJournal()
		if err != nil {
			return err
		}
		summary, err := journal.Verify(auditHead)
		if err != nil {
			return i18n.Errorf("журнал аудита не прошёл проверку: %v", err)
		}
		if summary.Records == 0 {
			cmd.Println(i18n.T("Журнал аудита пуст"))
			return nil
		}
		cmd.Println(i18n.Sprintf("Журнал аудита цел: записей %d, с %s по %s", summary.Records,
			summary.First.Local().Format("2006-01-02 15:04:05"), summary.Last.Local().Format("2006-01-02 15:04:05")))
		cmd.Println(i18n.Sprintf("Хеш последней записи: %s", summary.Head))
		return nil
	},
}

func init() {
	auditCmd.Flags().IntVarP(&auditLimit, "n", "n", 20, "сколько записей показать (0 — все)")
	auditCmd.Flags().StringVar(&auditRole, "role", "", "только записи этой роли")
	auditCmd.Flags().BoolVar(&auditDenied, "denied", false, "только отказы")
	auditVerifyCmd.Flags().StringVar(&auditHead, "head", "", "хеш записи, сохранённый вне машины: она должна быть в журнале")
	auditCmd.AddCommand(auditVerifyCmd)
	rootCmd.AddCommand(auditCmd)
}
