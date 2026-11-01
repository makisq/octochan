package learn

import (
	"math"
	"testing"
	"time"
)

var (
	connections = []string{"class Connection", "class Process", "relation Connection limits Process", "set Connection.maximum #"}
	hugepages   = []string{"class Memory", "class Setting", "relation Memory limits Setting", "set Memory.size #GB"}
	other       = []string{"class Log", "class Storage", "relation Log modifies Storage"}
)

func items(shape []string, n int, start time.Time, prefix string) []Item {
	out := make([]Item, n)
	for i := range out {
		out[i] = Item{ID: prefix + string(rune('a'+i)), Time: start.Add(time.Duration(i) * time.Hour), Shape: shape}
	}
	return out
}

// Порог как у вакуума: больше threshold + scale_factor × всех. Повторяющееся
// созревает, одиночное — шум, частое, но лёгкое — копится.
func TestJudge(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	start := now.Add(-48 * time.Hour)
	var all []Item
	all = append(all, items(connections, 12, start, "c")...)
	all = append(all, items(hugepages, 4, start, "h")...)
	all = append(all, Item{ID: "x", Time: start, Shape: other})
	// Тот же инцидент с другим лишним фактом — всё ещё та же структура.
	all = append(all, Item{ID: "c-near", Time: now, Shape: append(append([]string(nil), connections...), "set Process.load high")})

	cfg := Config{ScaleFactor: 0.2, Threshold: 5, Similarity: 0.75}
	j := Judge(all, cfg, now)
	if j.Total != 18 || math.Abs(j.Due-(5+0.2*18)) > 1e-9 {
		t.Fatalf("вес %v, порог %v", j.Total, j.Due)
	}
	verdicts := map[string]Verdict{}
	for _, c := range j.Clusters {
		verdicts[c.Shape[0]] = c.Verdict
	}
	if verdicts["class Connection"] != Ripe || verdicts["class Memory"] != Growing || verdicts["class Log"] != Noise {
		t.Fatalf("вердикты: %v", verdicts)
	}
	if len(j.Clusters[0].Items) != 13 {
		t.Fatalf("похожий инцидент не вошёл в кластер: %d", len(j.Clusters[0].Items))
	}

	// Тот же кластер, но старый: с давностью он весит меньше и не созревает.
	cfg.HalfLife = 24 * time.Hour
	old := items(connections, 12, now.Add(-30*24*time.Hour), "o")
	if j := Judge(old, cfg, now); j.Clusters[0].Verdict != Growing {
		t.Fatalf("старые инциденты держат кластер: вес %.3f, порог %.3f", j.Clusters[0].Weight, j.Due)
	}
	if w := Weight(now.Add(-24*time.Hour), now, 24*time.Hour); math.Abs(w-0.5) > 1e-12 {
		t.Fatalf("вес через half_life: %v", w)
	}
}
