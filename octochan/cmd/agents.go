package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"octochan/inventory"

	"github.com/spf13/cobra"
)

// Реестр агентов: машины, которые сами сообщают серверу о себе — имя,
// группы, переменные, адрес. Реестр — источник инвентаря agents://:
// scenarios берут цели из него, а ochan watch --source agents видит
// новый агент как новый хост.

// agentsFile — реестр в рабочем каталоге сервера; копируется на standby.
const agentsFile = "agents.json"

// Agent — запись реестра.
type Agent struct {
	Name    string         `json:"name"`
	Groups  []string       `json:"groups,omitempty"`
	Vars    map[string]any `json:"vars,omitempty"`
	Address string         `json:"address"`
	Version string         `json:"version,omitempty"`
	// Role — роль, в которой агент сообщил о себе первый раз: имя агента
	// принадлежит ей, другая роль его не займёт.
	Role  string        `json:"role,omitempty"`
	Every time.Duration `json:"every"`
	First time.Time     `json:"first"`
	Last  time.Time     `json:"last"`
}

// Online — агент сообщал о себе недавно: за три своих промежутка, но не
// меньше трёх минут.
func (a *Agent) Online(now time.Time) bool {
	window := 3 * a.Every
	if window < 3*time.Minute {
		window = 3 * time.Minute
	}
	return now.Sub(a.Last) <= window
}

type agentRegistry struct {
	Agents map[string]*Agent `json:"agents"`
}

func agentsPath() (string, error) {
	fm, err := mustFileManager()
	if err != nil {
		return "", err
	}
	return filepath.Join(fm.BaseDir(), agentsFile), nil
}

func loadAgents(path string) (*agentRegistry, error) {
	reg := &agentRegistry{Agents: map[string]*Agent{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return reg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, reg); err != nil {
		return nil, fmt.Errorf("реестр агентов %s испорчен: %w", path, err)
	}
	if reg.Agents == nil {
		reg.Agents = map[string]*Agent{}
	}
	return reg, nil
}

// changeAgents меняет реестр под замком: агенты сообщают о себе
// одновременно.
func changeAgents(change func(*agentRegistry) error) error {
	path, err := agentsPath()
	if err != nil {
		return err
	}
	lock := path + ".lock"
	for i := 0; ; i++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			break
		}
		if info, serr := os.Stat(lock); serr == nil && time.Since(info.ModTime()) > 30*time.Second {
			os.Remove(lock)
			continue
		}
		if i >= 200 {
			return errors.New("реестр агентов занят")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer os.Remove(lock)
	reg, err := loadAgents(path)
	if err != nil {
		return err
	}
	if err := change(reg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// agentSource — источник инвентаря agents:// (все) и agents://online
// (только те, что сообщали о себе недавно).
type agentSource struct{ online bool }

func (s agentSource) Hosts(context.Context) ([]inventory.Host, error) {
	path, err := agentsPath()
	if err != nil {
		return nil, err
	}
	reg, err := loadAgents(path)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var hosts []inventory.Host
	for _, a := range reg.Agents {
		online := a.Online(now)
		if s.online && !online {
			continue
		}
		vars := map[string]any{}
		for k, v := range a.Vars {
			vars[k] = v
		}
		vars["agent_address"] = a.Address
		vars["agent_online"] = online
		vars["agent_last_seen"] = a.Last.UTC().Format(time.RFC3339)
		hosts = append(hosts, inventory.Host{Name: a.Name, Groups: append([]string(nil), a.Groups...), Vars: vars})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	return hosts, nil
}

func openAgents(ref string) (inventory.Source, error) {
	switch strings.TrimPrefix(ref, "agents://") {
	case "":
		return agentSource{}, nil
	case "online":
		return agentSource{online: true}, nil
	}
	return nil, fmt.Errorf("источник %q: agents:// — все агенты, agents://online — только на связи", ref)
}

var (
	agentName   string
	agentGroups []string
	agentVars   []string
	agentEvery  time.Duration
	agentOnce   bool
)

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Реестр агентов: машины, которые сами сообщают серверу о себе",
	Long: "Агент — машина с ochan agent run: раз в --every он сообщает серверу имя,\n" +
		"группы и переменные, а сервер запоминает и его адрес. Реестр — источник\n" +
		"инвентаря agents:// (agents://online — только на связи): цели сценариев\n" +
		"берутся из него, а ochan watch --source agents видит новый агент как\n" +
		"новый хост и действует по политике.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := agentsPath()
		if err != nil {
			return err
		}
		reg, err := loadAgents(path)
		if err != nil {
			return err
		}
		if len(reg.Agents) == 0 {
			cmd.Println("Агентов нет: ochan agent run --server <адрес> --name <имя> на машине агента.")
			return nil
		}
		names := make([]string, 0, len(reg.Agents))
		for name := range reg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		now := time.Now()
		grants, _ := activeFree()
		for _, name := range names {
			a := reg.Agents[name]
			state := "на связи"
			if !a.Online(now) {
				state = "нет связи"
			}
			cmd.Printf("%-16s %-10s %-22s %-20s %s назад, роль %s\n", a.Name, state, a.Address, orNone(a.Groups),
				now.Sub(a.Last).Round(time.Second), orDash(a.Role))
			if free := freeLine(a.Name, grants); free != "" {
				cmd.Printf("%-16s isolation %s\n", "", free)
			}
		}
		return nil
	},
}

var agentsBeatCmd = &cobra.Command{
	Use:   "beat --name <имя> [--group g] [--var k=v]",
	Short: "Сообщить серверу о себе — это вызывает ochan agent run",
	Long: "Обновляет запись агента в реестре: группы, переменные, адрес, время.\n" +
		"Имя принадлежит роли, в которой агент сообщил о себе первый раз: другая\n" +
		"роль его не займёт. Право — agent.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := checkAgentName(agentName); err != nil {
			return err
		}
		vars, err := parseAgentVars(agentVars)
		if err != nil {
			return err
		}
		address := os.Getenv(EnvRemoteAddr)
		if address == "" {
			address = "local"
		}
		role := ""
		if currentRole != nil {
			role = currentRole.Name
		}
		now := time.Now().UTC()
		var fresh bool
		err = changeAgents(func(reg *agentRegistry) error {
			a, ok := reg.Agents[agentName]
			if ok && a.Role != "" && !strings.EqualFold(a.Role, role) {
				return fmt.Errorf("имя агента %s занято ролью %s: сообщать о нём может только она", agentName, a.Role)
			}
			if !ok {
				a = &Agent{Name: agentName, Role: role, First: now}
				reg.Agents[agentName] = a
				fresh = true
			}
			a.Groups, a.Vars, a.Address, a.Every, a.Last = splitPerms(agentGroups), vars, address, agentEvery, now
			a.Version = Version
			return nil
		})
		if err != nil {
			return err
		}
		if fresh {
			cmd.Printf("Агент %s зарегистрирован (%s)\n", agentName, address)
		} else {
			cmd.Printf("Агент %s на связи (%s)\n", agentName, address)
		}
		return nil
	},
}

var agentsForgetCmd = &cobra.Command{
	Use:   "forget <имя>",
	Short: "Убрать агента из реестра",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		err := changeAgents(func(reg *agentRegistry) error {
			if _, ok := reg.Agents[args[0]]; !ok {
				return fmt.Errorf("агента %s нет", args[0])
			}
			delete(reg.Agents, args[0])
			return nil
		})
		if err != nil {
			return err
		}
		cmd.Printf("Агент %s убран из реестра\n", args[0])
		return nil
	},
}

func checkAgentName(name string) error {
	if name == "" {
		return errors.New("--name: имя агента")
	}
	for _, c := range name {
		if !(c == '-' || c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return fmt.Errorf("имя агента %q: латинские буквы, цифры, - _ .", name)
		}
	}
	return nil
}

func parseAgentVars(list []string) (map[string]any, error) {
	vars := map[string]any{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("--var %q: нужен вид ключ=значение", kv)
		}
		vars[strings.TrimSpace(k)] = v
	}
	return vars, nil
}

func init() {
	inventory.Register("agents", openAgents)
	agentsBeatCmd.Flags().StringVar(&agentName, "name", "", "имя агента — имя хоста в инвентаре")
	agentsBeatCmd.Flags().StringSliceVar(&agentGroups, "group", nil, "группы агента")
	agentsBeatCmd.Flags().StringArrayVar(&agentVars, "var", nil, "переменная агента ключ=значение")
	agentsBeatCmd.Flags().DurationVar(&agentEvery, "every", time.Minute, "как часто агент сообщает о себе")
	agentsFreeCmd.Flags().StringVar(&freeRole, "role", "", "чьи права на выполнение модель берёт")
	agentsFreeCmd.Flags().StringVar(&freeFor, "for", "", "срок: 30m, 4h, 1d")
	agentsFreeCmd.Flags().StringVar(&freePolicy, "policy", "", "политика: что выполнять без человека")
	agentsFreeCmd.Flags().BoolVar(&freeRevoke, "revoke", false, "отозвать free агента")
	agentsCmd.AddCommand(agentsBeatCmd, agentsForgetCmd, agentsFreeCmd)
	rootCmd.AddCommand(agentsCmd)
}
