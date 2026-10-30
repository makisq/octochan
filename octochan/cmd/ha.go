package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"octochan/rbac"
	"octochan/remote"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Пара серверов primary/standby — remote/ha.go. Здесь — что копируется,
// настройки, команды ochan ha и подключение пары к ochan serve.

// replicatedPaths — состояние, которое standby держит копией: заявки,
// журнал операций, решения, политики, роли с паролями и ключом допусков,
// журнал аудита, инвентарь и реестр агентов, библиотека алиасов. Не
// копируются настройки узла (config.yaml, tls/, ha.json, ha.token),
// токены исполнителей и модули — их ставят на каждый узел.
var replicatedPaths = []string{"requests", "history", "decisions", "policies", "aliases", "scenarios",
	"audit.log", rbac.FileName, "inventory.yaml", "inventory.seen.json", agentsFile, triggersFile, triggerSecrets}

var (
	haInitNode string
	haInitRole string
	haPeers    []string
)

func haStatePath() (string, error) {
	fm, err := mustFileManager()
	if err != nil {
		return "", err
	}
	return filepath.Join(fm.BaseDir(), "ha.json"), nil
}

// haToken — общий секрет пары из ha.token_file.
func haToken() (string, error) {
	path := viper.GetString("ha.token_file")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("общий секрет пары %s: %w (его создаёт ochan ha init на первом узле, на второй его копируют)", path, err)
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 {
		return "", fmt.Errorf("общий секрет пары %s короче 32 символов", path)
	}
	return token, nil
}

// pairConfigured — у узла есть ha.json: он в паре.
func pairConfigured() bool {
	path, err := haStatePath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// newPairNode собирает узел пары для ochan serve.
func newPairNode(listen string, logger *log.Logger) (*remote.HA, error) {
	statePath, err := haStatePath()
	if err != nil {
		return nil, err
	}
	token, err := haToken()
	if err != nil {
		return nil, err
	}
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	self := viper.GetString("ha.self")
	if self == "" {
		self = listen
	}
	return remote.NewHA(remote.HAConfig{
		StatePath:      statePath,
		Base:           fm.BaseDir(),
		Paths:          replicatedPaths,
		Self:           self,
		Peer:           viper.GetString("ha.peer"),
		PeerTLS:        tlsSettings(),
		AllowPlaintext: servePlaintext,
		Token:          token,
		Every:          viper.GetDuration("ha.replicate_every"),
		RequireStandby: viper.GetBool("ha.require_standby"),
		Logger:         logger,
	})
}

// serveBarrier слушает барьер пары на 127.0.0.1: дочерние процессы этого
// узла сообщают через него, что пометка «выполняется» должна дойти до
// резерва. Адрес сервера может быть внешним, барьер — только свой.
func serveBarrier(ctx context.Context, ha *remote.HA) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	httpServer := &http.Server{Handler: ha.BarrierHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	go func() {
		<-ctx.Done()
		_ = httpServer.Close()
	}()
	return "http://" + listener.Addr().String() + "/v1/ha/barrier", nil
}

// readOnlyCommand — команда только читает: её выполняет и standby, и
// клиент может отправить её на любой узел пары.
func readOnlyCommand(args []string) bool {
	if _, ok := reply(args); ok {
		return true
	}
	findMu.Lock()
	found, _, err := rootCmd.Find(args)
	key := ""
	if err == nil {
		key = commandKey(found)
	}
	findMu.Unlock()
	if key == "" {
		return false
	}
	return rbac.Covers("read", key)
}

var haCmd = &cobra.Command{
	Use:   "ha",
	Short: "Пара серверов primary/standby: настройка, состояние, переключение",
	Long: "Два узла ochan serve: primary пишет, standby держит копию его состояния\n" +
		"(заявки, журнал, роли, аудит, политики, реестр агентов) и отвечает на\n" +
		"чтение. Консенсуса нет: переключение — ochan ha promote на standby,\n" +
		"решение человека. Эпоха — забор: узел, увидевший primary с эпохой больше\n" +
		"своей, перестаёт быть primary сам и откладывает то, что записал после\n" +
		"разрыва, в ha/diverged-….\n\n" +
		"Настройки — config.yaml: ha.peer (адрес второго узла), ha.self (адрес\n" +
		"этого узла для клиентов), ha.token_file, ha.replicate_every,\n" +
		"ha.require_standby.",
	Example: "ochan ha init --node a --role primary\nochan ha init --node b --role standby\n" +
		"ochan ha status\nochan ha status a.example:7443,b.example:7443\nochan ha promote",
}

var haInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Сделать этот узел узлом пары",
	Long: "Записывает ha.json узла: имя и роль. Первый узел — primary, второй —\n" +
		"standby. Общий секрет пары (ha.token_file) создаётся, если его нет:\n" +
		"скопируйте его на второй узел так же бережно, как ключ TLS.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if haInitRole != remote.RolePrimary && haInitRole != remote.RoleStandby {
			return fmt.Errorf("--role: primary или standby")
		}
		if haInitNode == "" {
			return fmt.Errorf("--node: имя узла")
		}
		path, err := haStatePath()
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s уже есть: узел уже в паре (ochan ha status)", path)
		}
		epoch := int64(0)
		if haInitRole == remote.RolePrimary {
			epoch = 1
		}
		st := &remote.HAState{Node: haInitNode, Role: haInitRole, Epoch: epoch, Since: time.Now().UTC(), By: whoAmI()}
		if err := remote.SaveHAState(path, st); err != nil {
			return err
		}
		tokenPath := viper.GetString("ha.token_file")
		if _, err := os.Stat(tokenPath); errors.Is(err, os.ErrNotExist) {
			buf := make([]byte, 32)
			if _, err := rand.Read(buf); err != nil {
				return err
			}
			if err := os.WriteFile(tokenPath, []byte(hex.EncodeToString(buf)+"\n"), 0o600); err != nil {
				return err
			}
			cmd.Printf("Общий секрет пары: %s (права 0600) — скопируйте его на второй узел\n", tokenPath)
		}
		cmd.Printf("Узел %s: %s, эпоха %d (%s)\n", st.Node, st.Role, st.Epoch, path)
		if viper.GetString("ha.peer") == "" {
			cmd.Println("Укажите адрес второго узла: ha.peer в config.yaml")
		}
		return nil
	},
}

var haStatusCmd = &cobra.Command{
	Use:   "status [host:port,...]",
	Short: "Состояние узлов пары",
	Long: "Без адресов — этот узел (ha.json) и второй (ha.peer). С адресами — что\n" +
		"говорят о себе эти узлы: так клиент видит, кто primary.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var addrs []string
		if len(args) == 1 {
			addrs = splitAddrs(args[0])
		} else {
			path, err := haStatePath()
			if err != nil {
				return err
			}
			st, err := remote.LoadHAState(path)
			if err != nil {
				return fmt.Errorf("этот узел не в паре: %w", err)
			}
			cmd.Printf("Этот узел: %s — %s, эпоха %d, с %s (%s)\n", st.Node, st.Role, st.Epoch,
				st.Since.Local().Format("2006-01-02 15:04:05"), orDash(st.By))
			for _, dir := range st.Diverged {
				cmd.Printf("Отложено расходящееся состояние: %s\n", dir)
			}
			for _, a := range []string{viper.GetString("ha.self"), viper.GetString("ha.peer")} {
				if a != "" {
					addrs = append(addrs, a)
				}
			}
		}
		for _, addr := range addrs {
			st, err := remote.NodeStatus(cmd.Context(), addr, tlsSettings(), remotePlain)
			if err != nil {
				cmd.Printf("%-24s нет ответа: %v\n", addr, err)
				continue
			}
			line := fmt.Sprintf("%-24s %s — %s, эпоха %d", addr, st.Node, st.Role, st.Epoch)
			if st.Conflict {
				line += ", КОНФЛИКТ: запись закрыта"
			}
			if st.Role == remote.RoleStandby {
				line += ", копия от " + formatSync(st.LastSync)
			}
			cmd.Println(line)
			if st.Error != "" {
				cmd.Printf("%-24s %s\n", "", st.Error)
			}
		}
		return nil
	},
}

var haPromoteCmd = &cobra.Command{
	Use:   "promote",
	Short: "Сделать этот узел primary: переключение пары",
	Long: "Эпоха узла становится на единицу больше известной. Работающий ochan serve\n" +
		"подхватывает её сам; бывший primary, увидев большую эпоху, отступает и\n" +
		"откладывает то, что успел записать после разрыва.\n\n" +
		"Консенсуса нет: решение — ваше. Если бывший primary жив и отрезан сетью,\n" +
		"до восстановления связи он будет принимать запись — остановите его или\n" +
		"отрежьте от клиентов, прежде чем повышать резерв.\n\n" +
		"Заявки, которые числились выполняемыми на бывшем primary, становятся\n" +
		"упавшими с неизвестным исходом: второй раз они не выполняются.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := haStatePath()
		if err != nil {
			return err
		}
		known := int64(0)
		if peer := viper.GetString("ha.peer"); peer != "" {
			if st, err := remote.NodeStatus(cmd.Context(), peer, tlsSettings(), remotePlain); err == nil {
				known = st.Epoch
				if st.Role == remote.RolePrimary {
					cmd.Printf("внимание: второй узел %s отвечает и он primary (эпоха %d): он отступит, увидев новую эпоху\n",
						st.Node, st.Epoch)
				}
			} else {
				cmd.Printf("Второй узел %s не отвечает: %v\n", peer, err)
			}
		}
		st, err := remote.Promote(path, whoAmI(), known)
		if err != nil {
			return err
		}
		cmd.Printf("Узел %s — primary, эпоха %d\n", st.Node, st.Epoch)
		return settleAfterPromote(cmd, st)
	},
}

// settleAfterPromote — заявки, которые выполнялись на прежнем primary:
// исход неизвестен, они упали.
func settleAfterPromote(cmd *cobra.Command, st *remote.HAState) error {
	store, err := requestStore()
	if err != nil {
		return err
	}
	all, err := store.List()
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.State != workspace.RequestRunning || r.Runner == nil {
			continue
		}
		if r.Runner.Node == st.Node && r.Runner.Epoch == st.Epoch {
			continue
		}
		failure := fmt.Errorf("исход неизвестен: заявка выполнялась (%s), когда узел %s перестал быть primary",
			r.Runner, orDash(r.Runner.Node))
		if err := store.MarkFailed(r, r.AppliedRef, failure); err != nil {
			return err
		}
		cmd.Printf("Заявка %s упала: %v\n", r.ID, failure)
		explainFailure(cmd, store, r)
	}
	return nil
}

func splitAddrs(text string) []string {
	var out []string
	for _, a := range strings.Split(text, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func formatSync(t time.Time) string {
	if t.IsZero() {
		return "ещё не было"
	}
	return t.Local().Format("2006-01-02 15:04:05") + fmt.Sprintf(" (%s назад)", time.Since(t).Round(time.Second))
}

func init() {
	haInitCmd.Flags().StringVar(&haInitNode, "node", "", "имя узла")
	haInitCmd.Flags().StringVar(&haInitRole, "role", "", "primary или standby")
	haStatusCmd.Flags().BoolVar(&remotePlain, "allow-plaintext", false, "разрешить опрос без ssl другой машины")
	haCmd.AddCommand(haInitCmd, haStatusCmd, haPromoteCmd)
	rootCmd.AddCommand(haCmd)
}
