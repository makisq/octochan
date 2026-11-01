"""Параметры колец архитектурной модели из журнала решений ochan (раздел 7).

Надёжность правил, порог well_True и условный счёт автора берутся из
журнала как есть; веса правила выбора — логистической регрессией по
исходам выборов модели, калибровка оценок — монотонной поправкой
«оценка → наблюдаемая доля» (rings.isotonic). Пока исходов мало (меньше
20 выборов с обоими исходами), веса и калибровка остаются прежними.

Результат — rings.json рядом с весами: та же модель после журнала, как
после обучения — новые веса.

    ochan fp journal export > journal.json
    python fit.py --model G:/fpm-llm/models/own-small-2 --journal journal.json
"""

import argparse
import json
import math

import rings


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--model", required=True, help="каталог своей модели: rings.json пишется туда")
    ap.add_argument("--journal", required=True, help="ochan fp journal export")
    args = ap.parse_args()

    with open(args.journal, encoding="utf-8-sig") as f:
        journal = json.load(f)
    params = rings.Params.load(args.model)
    params.prior = tuple(journal["prior"])
    params.well_true = journal["well_true"]
    params.rules = {rule: list(counts) for rule, counts in journal["rules"].items()}
    params.history = journal.get("history", {})

    choices = journal.get("choices", [])
    features = [(c["logprob"], c["l"], c["u"] - c["l"], 1.0) for c in choices]
    outcomes = [1 if c["correct"] else 0 for c in choices]
    weights = rings.fit_weights(features, outcomes)
    if weights:
        params.weights = weights
        params.calibration = []
        raw = [params.score(*x) for x in features]
        params.calibration = rings.isotonic(raw, outcomes)
    params.save(args.model)

    print(f"Правил {len(params.rules)}, выборов с исходом {len(choices)}, порог well_True {params.well_true}, "
          f"условный счёт {params.prior[0]:g}/{params.prior[1]:g}")
    for rule, (successes, uses) in sorted(params.rules.items()):
        lo, hi = params.rule(rule)
        print(f"  {rule:20} верных {successes:6.2f} из {uses:6.2f}  надёжность [{lo:.2f}; {hi:.2f}]")
    if weights:
        print("Веса правила выбора: " + ", ".join(f"β{i} {w:.3f}" for i, w in enumerate(params.weights)))
        print("Калибровка: " + ", ".join(f"{x:.2f} → {y:.2f}" for x, y in params.calibration))
    else:
        print("Веса и калибровка прежние: исходов выбора мало или все одинаковые")
    if not math.isfinite(sum(params.weights)):
        raise SystemExit("веса не сошлись")


if __name__ == "__main__":
    main()
