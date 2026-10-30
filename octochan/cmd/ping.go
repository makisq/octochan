package cmd

import (
	"strings"

	"github.com/spf13/cobra"
)

// Ответы, которым не нужен ни сценарий, ни процесс: проверка, что
// оркестратор жив и слышит. ping — по делу. SELECT 1 — так проверяют
// живость базы, и ochan отвечает так же, как psql. На «Ohayō, senpai!»
// ochan отвечает «Konbanwa»: у оркестратора, который не спит, всегда
// вечер.

const selectOneReply = " ?column? \n----------\n        1\n(1 row)\n"

// reply — готовый ответ на команду или false, если это обычная команда.
func reply(args []string) (string, bool) {
	text := strings.ToLower(strings.Join(strings.Fields(strings.Join(args, " ")), " "))
	text = strings.TrimSpace(strings.TrimSuffix(text, ";"))
	switch text {
	case "ping":
		return "pong\n", true
	case "select 1":
		return selectOneReply, true
	}
	if greeting(text) {
		return "Konbanwa\n", true
	}
	return "", false
}

// greeting — «доброе утро, семпай» в любом из привычных написаний.
func greeting(text string) bool {
	text = strings.NewReplacer("ō", "o", "ô", "o", "ou", "o", ",", " ", "!", " ", "、", " ", "！", " ").Replace(text)
	words := strings.Fields(text)
	switch strings.Join(words, " ") {
	case "ohayo senpai", "ohayo gozaimasu senpai", "おはよう 先輩", "おはようございます 先輩", "おはよう先輩":
		return true
	}
	return false
}

var pingCmd = &cobra.Command{
	Use:   "ping",
	Short: "Проверить, что ochan отвечает: pong",
	Long: "Отвечает pong. Через ochan remote <адрес> ping — проверка, что сервер\n" +
		"жив, принимает вход и выполняет команды. Так же отвечают SELECT 1 — как\n" +
		"psql — и приветствие Ohayō, senpai!",
	Example: "ochan ping\n" +
		"ochan remote orchestrator.example:7443 ping\n" +
		"ochan remote orchestrator.example:7443 'SELECT 1'",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cmd.Print("pong\n")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(pingCmd)
}
