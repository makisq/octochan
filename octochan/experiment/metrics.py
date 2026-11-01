"""Метрики эксперимента 9.2 по прогонам run.py (разделы 9.1, 9.2, А.5).

- ĉ — согласованность: доля пар прогонов с одинаковым планом; на одной
  записи (повторы) и между записями одной задачи (перефразировки, H1);
- p̂ — правильность: доля прогонов с задуманным планом I* задачи;
- градуированные ĉ и p̂ — 1 − δ, δ — взвешенное расстояние Жаккара по
  парам «вопрос → ответ» с весами задачи (А.5);
- H3 — на задаче без предпосылки: ошибка (отказ), а не догадка;
- ошибки вне отчёта — решения ячеек ответа, которых нет в отчёте (H2);
- H4 — доля задач, план которых меняется при смене версии модели;
- H6 — калибровка по журналу: ochan fp journal export (--journal);
- H7 — связь согласованности и правильности по записям;
- цена — время, обращения к модели, утверждения логики и выборы сети.

Разметка слепая (9.2): --blind пишет планы для разметчика без условия и
модели, в случайном порядке, с ключом отдельно.

    python metrics.py runs/*.jsonl --out results/report.md
"""

import argparse
import collections
import itertools
import json
import os
import random
import statistics
import sys

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from run import load_tasks  # noqa: E402


def delta(x, y, w):
    """Взвешенное расстояние Жаккара между планами (А.5); δ(∅, ∅) = 0."""
    a, b = set(x.items()), set(y.items())
    union = a | b
    if not union:
        return 0.0
    mu = lambda pairs: sum(w.get(q, 1) for q, _ in pairs)
    return mu(a ^ b) / mu(union)


def pairs(items):
    return list(itertools.combinations(items, 2))


def plan_key(row):
    return "⊥undef" if row["refused"] else json.dumps(row["plan"], sort_keys=True, ensure_ascii=False)


def summarize(rows, tasks):
    by_task = {t["id"]: t for t in tasks}
    formal = [r for r in rows if r["kind"] == "formal"]
    lacking = [r for r in rows if r["kind"] == "lacking"]
    out = {}

    same, para, graded_same, graded_para = [], [], [], []
    groups = collections.defaultdict(list)
    for r in formal:
        groups[(r["task"], r["writing"])].append(r)
    for (task, _), runs in groups.items():
        w = by_task[task]["weights"]
        for a, b in pairs(runs):
            same.append(plan_key(a) == plan_key(b))
            graded_same.append(1 - delta(a["plan"], b["plan"], w))
    for task in {r["task"] for r in formal}:
        first = {}
        for r in formal:
            if r["task"] == task and r["run"] == 0:
                first[r["writing"]] = r
        w = by_task[task]["weights"]
        for a, b in pairs(list(first.values())):
            para.append(plan_key(a) == plan_key(b))
            graded_para.append(1 - delta(a["plan"], b["plan"], w))
    mean = lambda xs: sum(xs) / len(xs) if xs else float("nan")
    out["ĉ на одной записи"] = (mean(same), mean(graded_same), len(same))
    out["ĉ между записями (H1)"] = (mean(para), mean(graded_para), len(para))

    right = [r["plan"] == by_task[r["task"]]["intended"] and not r["refused"] for r in formal]
    graded = [1 - delta(r["plan"], by_task[r["task"]]["intended"], by_task[r["task"]]["weights"]) for r in formal]
    out["p̂ по эталону"] = (mean(right), mean(graded), len(right))

    out["H3: без предпосылки — отказ"] = (mean([r["refused"] or not r["plan"] for r in lacking]), None, len(lacking))

    # Решение ячейки, вопроса которого нет в отчёте, — ошибка вне отчёта.
    outside = [any(target.split(".")[-1] not in r["plan"] for target in r["decisions"]) for r in formal if r["ok"]]
    out["ошибки вне отчёта (H2)"] = (mean(outside), None, len(outside))

    # H7: по записям — согласованность повторов и правильность.
    per_writing = []
    for (task, _), runs in groups.items():
        c = mean([plan_key(a) == plan_key(b) for a, b in pairs(runs)]) if len(runs) > 1 else 1.0
        p = mean([r["plan"] == by_task[task]["intended"] for r in runs])
        per_writing.append((c, p))
    if len(per_writing) > 2 and len({c for c, _ in per_writing}) > 1 and len({p for _, p in per_writing}) > 1:
        out["H7: корреляция ĉ и p̂ по записям"] = (statistics.correlation(*zip(*per_writing)), None, len(per_writing))
    else:
        out["H7: корреляция ĉ и p̂ по записям"] = (float("nan"), None, len(per_writing))

    out["цена: время на прогон, с"] = (mean([r["seconds"] for r in formal]), None, len(formal))
    out["цена: утверждений логики / выборов сети"] = (mean([r["forced"] for r in formal]),
                                                       mean([r["chosen"] for r in formal]), len(formal))
    return out


def versions(rows_by_model):
    """H4: доля задач, план которых (первый прогон первой записи) меняется
    между версиями модели."""
    plans = collections.defaultdict(dict)
    for model, rows in rows_by_model.items():
        for r in rows:
            if r["kind"] == "formal" and r["writing"] == 0 and r["run"] == 0:
                plans[r["task"]][model] = plan_key(r)
    changed = [len(set(v.values())) > 1 for v in plans.values() if len(v) > 1]
    return (sum(changed) / len(changed) if changed else float("nan")), len(changed)


def calibration(path):
    """H6: доля верных по интервалам оценок выбора из журнала."""
    with open(path, encoding="utf-8-sig") as f:
        journal = json.load(f)
    bins = collections.defaultdict(lambda: [0, 0])
    for c in journal.get("choices", []):
        k = min(int(c["score"] * 10), 9)
        bins[k][0] += c["correct"]
        bins[k][1] += 1
    return [(k / 10, (k + 1) / 10, right / total, total) for k, (right, total) in sorted(bins.items())]


def blind(rows, path, seed=7):
    """Планы для слепой разметки: без условия и модели, в случайном порядке;
    ключ — отдельным файлом."""
    items = [{"item": i, "task": r["task"], "plan": r["plan"], "refused": r["refused"]} for i, r in enumerate(rows)]
    keys = [{"item": i, "condition": r["condition"], "model": r["model"], "writing": r["writing"], "run": r["run"]}
            for i, r in enumerate(rows)]
    random.Random(seed).shuffle(items)
    with open(path, "w", encoding="utf-8") as f:
        for item in items:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")
    with open(path + ".key", "w", encoding="utf-8") as f:
        for key in keys:
            f.write(json.dumps(key, ensure_ascii=False) + "\n")


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("runs", nargs="+")
    ap.add_argument("--out", required=True)
    ap.add_argument("--journal", help="ochan fp journal export — для H6")
    ap.add_argument("--blind", help="файл планов для слепой разметки")
    args = ap.parse_args()

    tasks = load_tasks()
    rows = [json.loads(line) for path in args.runs for line in open(path, encoding="utf-8")]
    by_condition = collections.defaultdict(list)
    by_model = collections.defaultdict(list)
    for r in rows:
        by_condition[(r["condition"], r["model"])].append(r)
        by_model[r["model"]].append(r)

    with open(os.path.join(HERE, "plan.yaml"), encoding="utf-8") as f:
        plan = yaml.safe_load(f)
    lines = [f"# Эксперимент 9.2: результаты", "",
             f"План версии {plan['version']} от {plan['fixed']}; задач {len(tasks)}, прогонов {len(rows)}.", ""]
    for (condition, model), group in sorted(by_condition.items()):
        lines += [f"## {condition}, модель {model}", "", "| метрика | точно | 1 − δ | n |", "|---|---|---|---|"]
        for name, (value, graded, n) in summarize(group, tasks).items():
            fmt = lambda v: "—" if v is None else f"{v:.3f}"
            lines.append(f"| {name} | {fmt(value)} | {fmt(graded)} | {n} |")
        lines.append("")
    if len(by_model) > 1:
        share, n = versions(by_model)
        lines += [f"## H4: смена версии модели", "",
                  f"Доля задач, план которых меняется между версиями ({', '.join(sorted(by_model))}): "
                  f"{share:.3f} из {n} задач.", ""]
    if args.journal:
        lines += ["## H6: калибровка выбора по журналу", "", "| оценка | доля верных | выборов |", "|---|---|---|"]
        lines += [f"| {lo:.1f}–{hi:.1f} | {p:.2f} | {n} |" for lo, hi, p, n in calibration(args.journal)]
        lines.append("")
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    with open(args.out, "w", encoding="utf-8") as f:
        f.write("\n".join(lines))
    if args.blind:
        blind(rows, args.blind)
    print("\n".join(lines))


if __name__ == "__main__":
    main()
