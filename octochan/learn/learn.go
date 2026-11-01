// Package learn — когда своей модели учиться на инцидентах.
//
// Одиночный инцидент — шум: учить на нём модель — значит запоминать
// случайность. Учиться стоит тому, что повторяется: инцидентам одной
// структуры (fp.Shape — запись на ℒ без частных имён, разные хосты и числа
// — одна структура), и только когда их стало заметно много. Порог — как у
// вакуума в PostgreSQL: таблица идёт в вакуум, когда мёртвых строк больше
// autovacuum_vacuum_threshold + autovacuum_vacuum_scale_factor × строк
// таблицы. Здесь кластер созрел, когда его вес больше
//
//	threshold + scale_factor × вес всех инцидентов,
//
// а вес инцидента убывает с давностью (half_life): старые не держат
// кластер вечно. Минимум threshold не даёт двум случаям из пяти
// называться закономерностью; доля scale_factor растёт с числом
// инцидентов, как у большой таблицы.
//
// Пакет — чистая математика: инцидентов, файлов и модели он не знает.
package learn

import (
	"math"
	"sort"
	"time"

	"ochan/fp"
)

// Config — параметры обучения, incidents.learn в config.yaml.
type Config struct {
	// ScaleFactor — доля от веса всех инцидентов, от 0 до 1.
	ScaleFactor float64
	// Threshold — минимум сверх доли: столько инцидентов кластер должен
	// перевесить в любом случае.
	Threshold float64
	// Similarity — с какой похожестью (1 − δ, fp.Similarity) инцидент
	// входит в кластер.
	Similarity float64
	// HalfLife — за какой срок вес инцидента падает вдвое; 0 — не падает.
	HalfLife time.Duration
}

// Item — инцидент: его структура и когда он случился.
type Item struct {
	ID    string
	Time  time.Time
	Shape []string
}

// Verdict — что делать с кластером.
type Verdict string

const (
	// Ripe — кластер созрел: модель учится на нём.
	Ripe Verdict = "ripe"
	// Growing — повторяется, но порога ещё не перевесил.
	Growing Verdict = "growing"
	// Noise — одиночный инцидент: в обучение не идёт.
	Noise Verdict = "noise"
)

// Cluster — инциденты одной структуры. Shape — структура первого из них:
// с ней сравниваются остальные.
type Cluster struct {
	Shape   []string
	Items   []Item
	Weight  float64
	Verdict Verdict
}

// Judgement — кластеры и порог.
type Judgement struct {
	Clusters []*Cluster
	// Total — вес всех инцидентов, Due — порог: созрел кластер тяжелее.
	Total, Due float64
}

// Judge группирует инциденты по структуре и решает, какие кластеры
// созрели. Порядок детерминирован: инциденты — по времени, затем по
// номеру; инцидент входит в первый кластер, на структуру которого похож
// не меньше Similarity; кластеры в ответе — от тяжёлых к лёгким.
func Judge(items []Item, cfg Config, now time.Time) Judgement {
	sorted := append([]Item(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Time.Equal(sorted[j].Time) {
			return sorted[i].Time.Before(sorted[j].Time)
		}
		return sorted[i].ID < sorted[j].ID
	})
	var out Judgement
	for _, it := range sorted {
		w := Weight(it.Time, now, cfg.HalfLife)
		out.Total += w
		var home *Cluster
		for _, c := range out.Clusters {
			if fp.Similarity(c.Shape, it.Shape) >= cfg.Similarity {
				home = c
				break
			}
		}
		if home == nil {
			home = &Cluster{Shape: it.Shape}
			out.Clusters = append(out.Clusters, home)
		}
		home.Items = append(home.Items, it)
		home.Weight += w
	}
	out.Due = cfg.Threshold + cfg.ScaleFactor*out.Total
	for _, c := range out.Clusters {
		switch {
		case len(c.Items) == 1:
			c.Verdict = Noise
		case c.Weight > out.Due:
			c.Verdict = Ripe
		default:
			c.Verdict = Growing
		}
	}
	sort.SliceStable(out.Clusters, func(i, j int) bool { return out.Clusters[i].Weight > out.Clusters[j].Weight })
	return out
}

// Weight — вес инцидента по давности: 1 у свежего, половина через
// halfLife. Инцидент из будущего (часы разошлись) весит 1.
func Weight(at, now time.Time, halfLife time.Duration) float64 {
	if halfLife <= 0 || !now.After(at) {
		return 1
	}
	return math.Pow(0.5, float64(now.Sub(at))/float64(halfLife))
}
