package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"octochan/fp"
	"octochan/learn"
	"octochan/rbac"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Обучение своей модели на повторяющихся инцидентах (пакет learn):
//
//	incidents:
//	  learn:
//	    scale_factor: 0.2   # доля от всех инцидентов — как autovacuum_vacuum_scale_factor
//	    threshold: 5        # минимум сверх доли — как autovacuum_vacuum_threshold
//	    similarity: 0.8     # с какой похожестью структуры инцидент входит в кластер
//	    half_life: 180d     # за какой срок вес инцидента падает вдвое
//
// В обучении участвуют инциденты, у которых «что случилось» записано на ℒ
// (так их пишет своя модель): их структура — fp.Shape. Записи на
// естественном языке структуры не дают и только считаются.

func learnConfig() (learn.Config, error) {
	var cfg learn.Config
	var err error
	if cfg.ScaleFactor, err = fraction(viper.Get("incidents.learn.scale_factor")); err != nil {
		return cfg, fmt.Errorf("incidents.learn.scale_factor: %w", err)
	}
	if cfg.Similarity, err = fraction(viper.Get("incidents.learn.similarity")); err != nil {
		return cfg, fmt.Errorf("incidents.learn.similarity: %w", err)
	}
	if cfg.Threshold = viper.GetFloat64("incidents.learn.threshold"); cfg.Threshold < 0 {
		return cfg, fmt.Errorf("incidents.learn.threshold: %v — число инцидентов, не меньше 0", cfg.Threshold)
	}
	if cfg.HalfLife, err = parseDays(viper.GetString("incidents.learn.half_life")); err != nil {
		return cfg, fmt.Errorf("incidents.learn.half_life: %w", err)
	}
	return cfg, nil
}

// learnRecord — инцидент созревшего кластера в выгрузке для обучения;
// Weight — его вес с давностью.
type learnRecord struct {
	ID      string                   `json:"id"`
	Weight  float64                  `json:"weight"`
	Opened  time.Time                `json:"opened"`
	State   string                   `json:"state"`
	Outcome string                   `json:"outcome"`
	Summary string                   `json:"summary"`
	Steps   []workspace.IncidentStep `json:"steps"`
}

// learnCluster — созревший кластер; Similarity — с какой похожестью задача
// повторяет его структуру: так её узнаёт модель (llama/learn.py).
type learnCluster struct {
	Shape      []string      `json:"shape"`
	Weight     float64       `json:"weight"`
	Similarity float64       `json:"similarity"`
	Incidents  []learnRecord `json:"incidents"`
}

var incidentLearnExport string

var incidentLearnCmd = &cobra.Command{
	Use:   "learn [--export файл]",
	Short: "Обучение по инцидентам: какие повторяются и созрели для модели",
	Long: "Одиночный инцидент — шум, учить на нём модель — значит запоминать\n" +
		"случайность. Команда группирует инциденты по структуре записи на ℒ —\n" +
		"без частных имён: разные хосты и числа дают одну структуру — и решает,\n" +
		"какой кластер созрел. Порог — как у вакуума в PostgreSQL: кластер\n" +
		"созрел, когда его вес больше incidents.learn.threshold +\n" +
		"incidents.learn.scale_factor × вес всех инцидентов; вес инцидента падает\n" +
		"вдвое за incidents.learn.half_life. Похожесть — incidents.learn.similarity.\n\n" +
		"--export пишет созревшие кластеры в JSON Lines — материал для обучения\n" +
		"своей модели. Инциденты на естественном языке структуры не дают и в\n" +
		"обучении не участвуют.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := learnConfig()
		if err != nil {
			return err
		}
		opts, err := fpOptions(cmd, false)
		if err != nil {
			return err
		}
		store, err := incidentStore()
		if err != nil {
			return err
		}
		all, err := store.List()
		if err != nil {
			return err
		}
		isWord := func(w string) bool {
			if opts.Lexicon == nil {
				return false
			}
			_, ok := opts.Lexicon.Lookup(w)
			return ok
		}
		var items []learn.Item
		byID := map[string]*workspace.Incident{}
		natural := 0
		for _, inc := range all {
			r := fp.CheckText(inc.ID, inc.Summary, opts)
			if !r.InLanguage() {
				natural++
				continue
			}
			items = append(items, learn.Item{ID: inc.ID, Time: inc.Opened, Shape: fp.Shape(r.Program, isWord)})
			byID[inc.ID] = inc
		}

		now := time.Now()
		j := learn.Judge(items, cfg, now)
		cmd.Printf("Инцидентов на ℒ: %d; на естественном языке: %d — в обучении не участвуют\n", len(items), natural)
		cmd.Printf("Порог: больше %g + %g × %.1f = %.1f (incidents.learn.threshold + scale_factor × вес всех)\n",
			cfg.Threshold, cfg.ScaleFactor, j.Total, j.Due)
		labels := map[learn.Verdict]string{learn.Ripe: "созрел", learn.Growing: "копится"}
		noise := 0
		var ripe []learnCluster
		for _, c := range j.Clusters {
			if c.Verdict == learn.Noise {
				noise++
				continue
			}
			cmd.Printf("  %-8s инцидентов %d, вес %.1f — %s\n", labels[c.Verdict], len(c.Items), c.Weight, shapeText(c.Shape))
			if c.Verdict == learn.Ripe {
				out := learnCluster{Shape: c.Shape, Weight: c.Weight, Similarity: cfg.Similarity}
				for _, it := range c.Items {
					inc := byID[it.ID]
					out.Incidents = append(out.Incidents, learnRecord{ID: inc.ID, Weight: learn.Weight(it.Time, now, cfg.HalfLife),
						Opened: inc.Opened, State: string(inc.State),
						Outcome: string(inc.Result(rbac.LLM)), Summary: inc.Summary, Steps: inc.Steps})
				}
				ripe = append(ripe, out)
			}
		}
		cmd.Printf("Шум: одиночных инцидентов %d — в обучение не идут\n", noise)
		if incidentLearnExport == "" {
			return nil
		}
		var buf strings.Builder
		for _, c := range ripe {
			line, err := json.Marshal(c)
			if err != nil {
				return err
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		if err := os.WriteFile(incidentLearnExport, []byte(buf.String()), 0o600); err != nil {
			return err
		}
		cmd.Printf("Созревших кластеров в %s: %d\n", incidentLearnExport, len(ripe))
		return nil
	},
}

// shapeText — структура кратко: отношения, иначе первые факты.
func shapeText(shape []string) string {
	var relations []string
	for _, f := range shape {
		if strings.HasPrefix(f, "relation ") {
			relations = append(relations, strings.TrimPrefix(f, "relation "))
		}
	}
	if len(relations) == 0 {
		relations = shape
	}
	if len(relations) > 3 {
		relations = append(relations[:3:3], "…")
	}
	return strings.Join(relations, "; ")
}

func init() {
	incidentLearnCmd.Flags().StringVar(&incidentLearnExport, "export", "", "записать созревшие кластеры в файл (JSON Lines)")
	incidentCmd.AddCommand(incidentLearnCmd)
}
