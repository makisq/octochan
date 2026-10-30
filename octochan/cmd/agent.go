package cmd

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"

	"octochan/remote"

	"github.com/spf13/cobra"
)

var (
	agentServer     string
	agentRole       string
	agentIsolation  string
	agentFreeRole   string
	agentFreeFor    string
	agentFreePolicy string
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Машина-агент: сообщать серверу о себе",
}

var agentRunCmd = &cobra.Command{
	Use:   "run --server <host:port[,host:port]> --name <имя> [--group g] [--var k=v]",
	Short: "Сообщать серверу о себе раз в --every",
	Long: "Входит на сервер в роли --role (по умолчанию agent; ей нужно право agent),\n" +
		"раз в --every выполняет там ochan agents beat с именем, группами и\n" +
		"переменными этой машины. Логин и пароль — из " + EnvRemoteLogin + " и\n" +
		EnvRemotePassword + ": спросить некого. Пара серверов — адреса через\n" +
		"запятую: агент переживает её переключение. --once — один раз, для cron.",
	Args: cobra.NoArgs,
	Example: "ochan agent run --server a.example:7443,b.example:7443 --name db7 --group db,db_new \\\n" +
		"  --var svm_ci=CI007 --every 1m",
	RunE: func(cmd *cobra.Command, _ []string) error {
		if agentServer == "" {
			return fmt.Errorf("--server: адрес сервера")
		}
		if err := checkAgentName(agentName); err != nil {
			return err
		}
		if _, err := parseAgentVars(agentVars); err != nil {
			return err
		}
		if agentEvery < 10*time.Second {
			return fmt.Errorf("--every %s: не чаще раза в 10 секунд", agentEvery)
		}
		remoteRole = agentRole
		pool, err := openSession(cmd, agentServer, bufio.NewReader(strings.NewReader("")))
		if err != nil {
			return fmt.Errorf("%w (логин и пароль агента — %s и %s)", err, EnvRemoteLogin, EnvRemotePassword)
		}
		defer pool.Logout(context.Background())

		if agentIsolation != "natural" && agentIsolation != "free" {
			return fmt.Errorf("--isolation %q: natural или free", agentIsolation)
		}
		args := []string{"agents", "beat", "--name", agentName, "--every", agentEvery.String()}
		for _, g := range agentGroups {
			args = append(args, "--group", g)
		}
		for _, v := range agentVars {
			args = append(args, "--var", v)
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		freeAsked := false
		for {
			result, err := pool.Exec(ctx, args, "", false)
			switch {
			case err != nil:
				if agentOnce {
					return err
				}
				cmd.PrintErrf("%s  сервер: %v\n", time.Now().Format("15:04:05"), err)
			case result.Code != 0:
				if agentOnce {
					return fmt.Errorf("сервер отказал: %s", strings.TrimSpace(result.Stderr))
				}
				cmd.PrintErrf("%s  сервер отказал: %s\n", time.Now().Format("15:04:05"), strings.TrimSpace(result.Stderr))
			default:
				cmd.Printf("%s  %s\n", time.Now().Format("15:04:05"), strings.TrimSpace(result.Stdout))
				if agentIsolation == "free" && !freeAsked {
					// free включается один раз, после того как агент в
					// реестре: выдают тому, кто сообщил о себе.
					freeAsked = true
					if err := askFree(ctx, cmd, pool); err != nil {
						return err
					}
				}
			}
			if agentOnce {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(agentEvery):
			}
		}
	},
}

func init() {
	f := agentRunCmd.Flags()
	f.StringVar(&agentServer, "server", "", "адрес сервера; пара — через запятую")
	f.StringVar(&agentName, "name", "", "имя агента — имя хоста в инвентаре")
	f.StringSliceVar(&agentGroups, "group", nil, "группы агента")
	f.StringArrayVar(&agentVars, "var", nil, "переменная агента ключ=значение")
	f.DurationVar(&agentEvery, "every", time.Minute, "как часто сообщать о себе")
	f.BoolVar(&agentOnce, "once", false, "сообщить один раз и выйти")
	f.StringVar(&agentRole, "role", "agent", "роль агента на сервере")
	f.StringVar(&agentIsolation, "isolation", "natural", "natural — модель только предлагает; free — выполняет по политике (только master, только HA-AI)")
	f.StringVar(&agentFreeRole, "free-role", "", "free: чьи права на выполнение модель берёт")
	f.StringVar(&agentFreeFor, "free-for", "", "free: срок, например 4h")
	f.StringVar(&agentFreePolicy, "free-policy", "", "free: политика — что выполнять без человека")
	f.BoolVar(&remotePlain, "allow-plaintext", false, "разрешить подключение без ssl к другой машине")
	agentCmd.AddCommand(agentRunCmd)
	rootCmd.AddCommand(agentCmd)
}

// askFree включает free агента на сервере: ochan agents free в сессии
// агента. Сервер пустит, только если сессия — master и узел — primary
// контура HA-AI; иначе агент не работает вовсе, а не молча без free.
func askFree(ctx context.Context, cmd *cobra.Command, pool *remote.Pool) error {
	args := []string{"agents", "free", agentName, "--role", agentFreeRole, "--for", agentFreeFor,
		"--policy", agentFreePolicy}
	result, err := pool.Exec(ctx, args, "", false)
	if err != nil {
		return fmt.Errorf("free: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("free: %s", strings.TrimSpace(result.Stderr))
	}
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		if strings.HasPrefix(line, "Агент ") {
			cmd.Printf("%s  %s\n", time.Now().Format("15:04:05"), line)
		}
	}
	return nil
}
