package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"octochan/fp"
	"octochan/i18n"
	"octochan/rbac"
	"octochan/report"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Почта ochan.
//
//	mail:
//	  lang: auto                      # auto | formal | natural
//	  operator: operators             # кто «оператор»: --to operator
//	  groups:
//	    operators: [ops@corp.example]
//	    dba: [dba@corp.example]
//
// Агенты пишут друг другу только формальным промптом: в любом режиме и в
// любой изоляции, free тоже. Людям — на языке режима: natural —
// естественным языком, иначе формальным промптом; mail.lang задаёт язык
// явно. Своя модель (llm.mode: embedded) знает только формальный промпт:
// её письма — формальные всегда, что бы ни стояло в mail.lang.

const mailDir = "mail"

type mailLang string

const (
	langFormal  mailLang = "formal"
	langNatural mailLang = "natural"
)

func (l mailLang) russian() string {
	if l == langNatural {
		return i18n.T("естественный язык")
	}
	return i18n.T("формальный промпт")
}

// mailLanguage — на каком языке ochan и модель пишут людям.
func mailLanguage() (mailLang, error) {
	mode, err := llmMode()
	if err != nil {
		return "", err
	}
	switch text := strings.ToLower(strings.TrimSpace(viper.GetString("mail.lang"))); text {
	case "", "auto":
		if mode == llmNatural {
			return langNatural, nil
		}
		return langFormal, nil
	case string(langFormal), string(langNatural):
		return mailLang(text), nil
	default:
		return "", fmt.Errorf("mail.lang: %q, допустимо auto, formal или natural", text)
	}
}

// mailLanguageLine — язык писем одной строкой для ochan llm.
func mailLanguageLine() string {
	lang, err := mailLanguage()
	if err != nil {
		return err.Error()
	}
	source := i18n.T("по режиму")
	if text := strings.TrimSpace(viper.GetString("mail.lang")); text != "" && text != "auto" {
		source = "mail.lang: " + text
	}
	return i18n.Sprintf("письма людям — %s (%s); агенты друг другу — только формальный промпт", lang.russian(), source)
}

func mailbox() (*workspace.Mailbox, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return workspace.NewMailbox(filepath.Join(fm.BaseDir(), mailDir))
}

// senderIsModel — пишет модель: роль llm.
func senderIsModel() bool { return currentRole != nil && currentRole.Name == rbac.LLM }

// mailGroup — адреса группы mail.groups; operator — группа mail.operator,
// а без неё — инженерная группа инцидентов или defaults.smtp_to.
func mailGroup(name string) ([]string, bool) {
	if name == "operator" {
		if group := viper.GetString("mail.operator"); group != "" {
			return mailGroup(group)
		}
		if to := incidentEngineers(""); len(to) > 0 {
			return to, true
		}
		return nil, false
	}
	to := viper.GetStringSlice("mail.groups." + strings.ToLower(name))
	return to, len(to) > 0
}

type recipients struct {
	agents []string
	emails []string
}

// resolveRecipients — кому письмо: агентам в ящики, людям по почте.
// Модель пишет только агентам и группам из настроек: адрес она выдумать
// не может.
func resolveRecipients(names []string) (recipients, error) {
	var r recipients
	path, err := agentsPath()
	if err != nil {
		return r, err
	}
	reg, err := loadAgents(path)
	if err != nil {
		return r, err
	}
	seen := map[string]bool{}
	add := func(list *[]string, v string) {
		if !seen[v] {
			seen[v] = true
			*list = append(*list, v)
		}
	}
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		switch {
		case name == "":
			continue
		case strings.Contains(name, "@"):
			if senderIsModel() {
				return r, fmt.Errorf("адрес %s: модель пишет только агентам и группам из mail.groups, адресов она не выбирает", name)
			}
			add(&r.emails, name)
		case reg.Agents[name] != nil:
			add(&r.agents, name)
		default:
			to, ok := mailGroup(name)
			if !ok {
				return r, fmt.Errorf("адресата %q нет: это не агент (ochan agents), не группа mail.groups и не адрес", name)
			}
			for _, a := range to {
				add(&r.emails, a)
			}
		}
	}
	if len(r.agents)+len(r.emails) == 0 {
		return r, fmt.Errorf("кому письмо — --to: агент, группа mail.groups или адрес")
	}
	return r, nil
}

// requiredLanguage — на каком языке должно быть письмо. Агентам — только
// формальным промптом. Своя модель — только формальным промптом.
func requiredLanguage(r recipients) (mailLang, error) {
	mode, err := llmMode()
	if err != nil {
		return "", err
	}
	if len(r.agents) > 0 || (mode == llmEmbedded && senderIsModel()) {
		return langFormal, nil
	}
	return mailLanguage()
}

// checkFormalMessage — письмо на языке: формальный промпт без ⊥syn. Письмо
// модели проверяется строже: ни комментариев, ни символов вне ASCII.
func checkFormalMessage(cmd *cobra.Command, body, why string) (*fp.Result, error) {
	return checkFormalText(cmd, i18n.T("письмо не на языке"), body, why)
}

// checkFormalText — текст на языке; title — что не на языке.
func checkFormalText(cmd *cobra.Command, title, body, why string) (*fp.Result, error) {
	opts, err := fpOptions(cmd, false)
	if err != nil {
		return nil, err
	}
	var result *fp.Result
	if senderIsModel() {
		result = fp.CheckModelText("письмо", body, opts)
	} else {
		result = fp.CheckText("письмо", body, opts)
	}
	if result.InLanguage() {
		return result, nil
	}
	var lines []string
	for _, d := range result.Syntax() {
		lines = append(lines, "  "+d.String())
	}
	return nil, fmt.Errorf("%s: %s\n%s", title, why, strings.Join(lines, "\n"))
}

// checkModelSays — своя модель (llm.mode: embedded) пишет только
// формальным промптом: не только письма, но и записи инцидента — что
// случилось, заметки, решение, причина эскалации. Их читают инженеры.
// Внешняя модель (protocol) пишет записи как есть: это записи, а не письма.
func checkModelSays(cmd *cobra.Command, text, what string) error {
	if !senderIsModel() {
		return nil
	}
	mode, err := llmMode()
	if err != nil || mode != llmEmbedded {
		return err
	}
	_, err = checkFormalText(cmd, i18n.T("запись инцидента не на языке"), text,
		i18n.Sprintf("%s — своя модель (llm.mode: embedded) пишет только формальным промптом", what))
	return err
}

var (
	mailTo      []string
	mailSubject string
	mailAgent   string
)

var mailCmd = &cobra.Command{
	Use:   "mail [номер]",
	Short: "Почта: письма агентов друг другу и людям",
	Long: "Агент пишет агенту — письмо ложится в его ящик; людям (группы mail.groups,\n" +
		"адреса) — уходит по почте. Агенты друг другу пишут только формальным\n" +
		"промптом, людям — на языке режима: llm.mode: natural — естественным\n" +
		"языком, иначе формальным промптом (mail.lang задаёт язык явно). Своя\n" +
		"модель (llm.mode: embedded) пишет только формальным промптом.\n\n" +
		"Без аргументов — письма: ящик агента (--agent или агент модели, ochan-mcp\n" +
		"--agent), без агента — все. С номером — письмо целиком.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		box, err := mailbox()
		if err != nil {
			return err
		}
		agent := mailAgent
		if agent == "" {
			agent = os.Getenv(EnvAgent)
		}
		// Модель читает только ящик своего агента и свои письма.
		if senderIsModel() {
			if own := os.Getenv(EnvAgent); own == "" || agent != own {
				return fmt.Errorf("модель читает только ящик своего агента: ochan-mcp --agent")
			}
		}
		if len(args) == 1 {
			msg, err := box.Get(args[0])
			if err != nil {
				return err
			}
			if senderIsModel() && msg.FromAgent != agent && len(workspace.MessagesFor([]*workspace.Message{msg}, agent)) == 0 {
				return fmt.Errorf("письмо %s не в ящике агента %s и написано не им", msg.ID, agent)
			}
			showMessage(cmd, msg)
			return nil
		}
		all, err := box.List()
		if err != nil {
			return err
		}
		if agent != "" {
			all = workspace.MessagesFor(all, agent)
		}
		if len(all) == 0 {
			if agent != "" {
				cmd.Println(i18n.Sprintf("В ящике агента %s писем нет", agent))
			} else {
				cmd.Println(i18n.T("Писем нет"))
			}
			return nil
		}
		for _, msg := range all {
			cmd.Printf("%s  %s  %-22s → %-24s %-8s %s\n", msg.Time.Local().Format("2006-01-02 15:04"), msg.ID,
				msg.From, strings.Join(msg.To, ", "), msg.Lang, messageTitle(msg))
		}
		return nil
	},
}

// messageTitle — тема письма, а у формального — его цель.
func messageTitle(msg *workspace.Message) string {
	if msg.Subject != "" {
		return msg.Subject
	}
	if prog, _ := fp.Parse("письмо", msg.Body); prog != nil && prog.Goal != nil {
		return "goal: " + prog.Goal.Word
	}
	first, _, _ := strings.Cut(strings.TrimSpace(msg.Body), "\n")
	return first
}

func showMessage(cmd *cobra.Command, msg *workspace.Message) {
	cmd.Printf("Письмо:  %s\n", msg.ID)
	cmd.Printf("Время:   %s\n", msg.Time.Local().Format("2006-01-02 15:04:05"))
	cmd.Printf("От:      %s\n", msg.From)
	cmd.Printf("Кому:    %s\n", strings.Join(msg.To, ", "))
	if len(msg.Agents) > 0 {
		cmd.Printf("В ящики: %s\n", strings.Join(msg.Agents, ", "))
	}
	if len(msg.Emails) > 0 {
		state := i18n.T("ушло")
		if msg.Error != "" {
			state = i18n.Sprintf("не ушло: %s", msg.Error)
		}
		cmd.Printf("Почтой:  %s — %s\n", strings.Join(msg.Emails, ", "), state)
	}
	cmd.Printf("Язык:    %s\n", mailLang(msg.Lang).russian())
	if msg.Subject != "" {
		cmd.Printf("Тема:    %s\n", msg.Subject)
	}
	cmd.Printf("\n%s", msg.Body)
	if !strings.HasSuffix(msg.Body, "\n") {
		cmd.Println()
	}
}

// messageArg — текст письма: «-» — stdin, путь к файлу — его содержимое,
// иначе сам аргумент.
func messageArg(arg string) (string, error) {
	if arg != "-" && !strings.Contains(arg, "\n") {
		if info, err := os.Stat(arg); err == nil && info.Mode().IsRegular() {
			data, err := os.ReadFile(arg)
			return string(data), err
		}
	}
	return textArg(arg)
}

var mailSendCmd = &cobra.Command{
	Use:   "send --to <агент|группа|адрес> [--subject тема] <текст | файл | ->",
	Short: "Написать агенту или людям",
	Long: "--to — агент из реестра (письмо ляжет в его ящик), группа mail.groups,\n" +
		"operator (группа mail.operator) или адрес; через запятую — нескольким.\n" +
		"Модель (роль llm) пишет только агентам и группам: адреса она не выбирает.\n\n" +
		"Язык проверяется до отправки: письмо агенту и письмо своей модели —\n" +
		"формальный промпт без ⊥syn; людям — на языке режима. У формального\n" +
		"письма темы нет: тема — его цель.",
	Args: cobra.ExactArgs(1),
	Example: "ochan mail send --to db8 report.fp\n" +
		"ochan llm generate --prompt задача.fp | ochan mail send --to operators -",
	RunE: func(cmd *cobra.Command, args []string) error {
		body, err := messageArg(args[0])
		if err != nil {
			return err
		}
		if strings.TrimSpace(body) == "" {
			return fmt.Errorf("письмо пустое")
		}
		to, err := resolveRecipients(mailTo)
		if err != nil {
			return err
		}
		lang, err := requiredLanguage(to)
		if err != nil {
			return err
		}
		if lang == langFormal {
			why := i18n.T("людям пишут формальным промптом — mail.lang и режим модели")
			switch {
			case len(to.agents) > 0:
				why = i18n.T("агентам пишут только формальным промптом")
			case senderIsModel():
				if mode, _ := llmMode(); mode == llmEmbedded {
					why = i18n.T("своя модель (llm.mode: embedded) пишет только формальным промптом")
				}
			}
			if _, err := checkFormalMessage(cmd, body, why); err != nil {
				return err
			}
			if mailSubject != "" {
				return fmt.Errorf("--subject: у формального письма темы нет, тема — его цель (goal:)")
			}
		}

		from := actor()
		agent := os.Getenv(EnvAgent)
		if agent != "" {
			from = i18n.Sprintf("%s, агент %s", from, agent)
		}
		msg := &workspace.Message{From: from, FromAgent: agent, To: mailTo, Agents: to.agents, Emails: to.emails,
			Lang: string(lang), Subject: mailSubject, Body: body}
		box, err := mailbox()
		if err != nil {
			return err
		}
		if err := box.Create(msg); err != nil {
			return err
		}
		if len(to.emails) > 0 {
			subject := mailSubject
			if subject == "" {
				subject = "[ochan] " + messageTitle(msg) + " — " + from
			}
			if err := mailSender(mailConfig(to.emails), report.Mail{Subject: subject, Body: body}); err != nil {
				msg.Error = err.Error()
			}
			if err := box.Save(msg); err != nil {
				return err
			}
		}

		cmd.Printf("Письмо %s (%s)\n", msg.ID, lang.russian())
		if len(to.agents) > 0 {
			cmd.Printf("  в ящики: %s\n", strings.Join(to.agents, ", "))
		}
		if len(to.emails) > 0 {
			if msg.Error != "" {
				cmd.Printf("  почтой: %s — не ушло: %s\n", strings.Join(to.emails, ", "), msg.Error)
				return fmt.Errorf("письмо людям не ушло: %s", msg.Error)
			}
			cmd.Printf("  почтой: %s — ушло\n", strings.Join(to.emails, ", "))
		}
		return nil
	},
}

func init() {
	mailCmd.Flags().StringVar(&mailAgent, "agent", "", "ящик агента")
	mailSendCmd.Flags().StringSliceVar(&mailTo, "to", nil, "агент, группа mail.groups, operator или адрес; через запятую")
	mailSendCmd.Flags().StringVar(&mailSubject, "subject", "", "тема письма на естественном языке")
	mailCmd.AddCommand(mailSendCmd)
	rootCmd.AddCommand(mailCmd)
}
