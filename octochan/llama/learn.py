"""Обучение своей модели на повторяющихся инцидентах (ochan incident learn).

Одиночный инцидент — шум; созревший кластер — закономерность, и модель
учится ей так же, как у неё устроено всё остальное: логико-структурно, со
статистикой только там, где она есть по смыслу.

- Обобщение — структура кластера без частных имён: факты logic.shape,
  которые есть больше чем у половины его инцидентов. Инциденты — частные
  случаи обобщения (стрелки Hom), их имена и числа — переменные.
- Правила на структуре — кандидаты: для инцидента с исходом логика модели
  строит свой план (те же правила, что при ответе), и каждое правило плана
  получает исход инцидента — решила модель сама (успех) или отдала
  инженерам: сама или по сроку (неудача). Решённое человеком и открытое
  исхода модели не говорит и не считается. Вес исхода падает с давностью,
  как в кластере. Без исходов у кандидата широкий интервал условного счёта
  автора; подтверждения его сужают.
- Задачу той же структуры модель узнаёт сама (rings.Params.cluster_of): её
  кольцо 1 берёт надёжность правил на этой структуре, а структура, которую
  модель решает плохо, — вопрос оператору, а не новая попытка.

Кластеры пересчитываются из выгрузки целиком: не созревший больше кластер
(старые инциденты потеряли вес) уходит из параметров. Каждая запись —
новая версия rings.json (rings.versions/), откат — --rollback.

--pairs — по желанию: материал для дообучения сети — пары задача → ответ
логики по инцидентам, которые модель решила сама, с примесью базового
корпуса (--replay), чтобы сеть не забыла остальное. Дообучение —
train_own.py --init: новые веса — в новый каталог, прежние остаются.

    ochan incident learn --export ripe.jsonl
    python learn.py --model G:/fpm-llm/models/own-small-2 --clusters ripe.jsonl
    python learn.py --model G:/fpm-llm/models/own-small-2 --versions
    python learn.py --model G:/fpm-llm/models/own-small-2 --rollback
"""

import argparse
import hashlib
import json
import os
import random
from collections import Counter

import fpmtok
import logic
import own
import rings

# Исход инцидента для модели: решила сама — успех; отдала инженерам сама
# или по сроку — неудача. Остальное исхода модели не говорит.
OUTCOMES = {"model": 1, "gave_up": 0, "timeout": 0}


def generalization(shapes):
    """Обобщение кластера: факты, которые есть больше чем у половины."""
    count = Counter(f for s in shapes for f in set(s))
    return sorted(f for f, n in count.items() if 2 * n > len(shapes))


def key_of(shape):
    return hashlib.sha256("\n".join(shape).encode("utf-8")).hexdigest()[:12]


def plan_rules(params, summary):
    """Правила, которыми модель решает задачу инцидента: вывод её логики и
    сужения по истории. Задача без предпосылки — правил нет."""
    if logic.missing(summary):
        return set()
    plan = logic.AnswerPlan(summary)
    rules = {r for d in plan.decisions for r in d.rules}
    return rules | {r for d, _ in own.narrowings(params, plan) for r in d.rules}


def learn(params, rows, is_word):
    """Кластеры параметров модели из выгрузки: ключ -> кластер."""
    clusters = {}
    for row in rows:
        incidents = row["incidents"]
        shape = generalization([logic.shape(i["summary"], is_word) for i in incidents])
        outcomes, rules = [0.0, 0.0], {}
        for inc in incidents:
            ok = OUTCOMES.get(inc["outcome"])
            if ok is None:
                continue
            w = inc.get("weight", 1.0)
            outcomes[0] += ok * w
            outcomes[1] += w
            for r in plan_rules(params, inc["summary"]):
                counts = rules.setdefault(r, [0.0, 0.0])
                counts[0] += ok * w
                counts[1] += w
        clusters[key_of(shape)] = {"shape": shape, "similarity": row["similarity"],
                                   "instances": [i["id"] for i in incidents], "outcomes": outcomes, "rules": rules}
    return clusters


def pairs(rows, replay, share, seed=7):
    """Пары для дообучения: задачи инцидентов, которые модель решила сама,
    с ответом логики; примесь базового корпуса — доля share среди пар
    обучения и отложенные базового — смотреть, не забыла ли сеть остальное.
    Возвращает (пары, сколько из инцидентов)."""
    rng = random.Random(seed)
    own_pairs = [{"task": i["summary"], "answer": logic.AnswerPlan(i["summary"]).text(), "split": "train"}
                 for row in rows for i in row["incidents"]
                 if OUTCOMES.get(i["outcome"]) == 1 and not logic.missing(i["summary"])]
    if not own_pairs:
        return [], 0
    base = {"train": [], "test": []}
    if replay:
        with open(replay, encoding="utf-8") as f:
            for line in f:
                r = json.loads(line)
                base[r.get("split", "train")].append(r)
    need = round(len(own_pairs) * share / (1 - share)) if share < 1 else len(base["train"])
    mixed = own_pairs + rng.sample(base["train"], min(need, len(base["train"])))
    mixed += rng.sample(base["test"], min(500, len(base["test"])))
    rng.shuffle(mixed)
    return mixed, len(own_pairs)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--model", required=True, help="каталог своей модели: rings.json пишется туда")
    ap.add_argument("--clusters", help="ochan incident learn --export: созревшие кластеры (JSON Lines)")
    ap.add_argument("--pairs", help="записать пары для дообучения сети (train_own.py --init)")
    ap.add_argument("--replay", help="базовый корпус для примеси к парам")
    ap.add_argument("--replay-share", type=float, default=0.9, help="доля базового корпуса среди пар обучения")
    ap.add_argument("--versions", action="store_true", help="версии параметров модели")
    ap.add_argument("--rollback", nargs="?", const=-1, type=int, metavar="N",
                    help="откатить параметры к версии N (без N — к предыдущей)")
    args = ap.parse_args()

    if args.versions:
        log = rings.versions(args.model)
        for v in log:
            print(f"{v['version']:4}  {v['time'] or '—':25}  {v['note']}")
        if not log:
            print("Версий нет: параметры ещё не пересчитывались (fit.py, learn.py)")
        return
    if args.rollback is not None:
        try:
            to, version = rings.rollback(args.model, None if args.rollback < 0 else args.rollback)
        except ValueError as e:
            raise SystemExit(str(e))
        print(f"Параметры откачены к версии {to}: это версия {version}")
        return
    if not args.clusters:
        raise SystemExit("нужно --clusters, --versions или --rollback")
    if not 0 <= args.replay_share < 1 and args.pairs:
        raise SystemExit("--replay-share — доля от 0 до 1, не включая 1")

    tok = fpmtok.Tokenizer.load(args.model)
    with open(args.clusters, encoding="utf-8-sig") as f:
        rows = [json.loads(line) for line in f if line.strip()]
    params = rings.Params.load(args.model)
    before = params.clusters
    params.clusters = learn(params, rows, tok.closed.__contains__)
    new = [k for k in params.clusters if k not in before]
    gone = [k for k in before if k not in params.clusters]
    print(f"Созревших кластеров {len(params.clusters)}: новых {len(new)}, прежних {len(params.clusters) - len(new)}, "
          f"ушло {len(gone)}")
    for key, c in params.clusters.items():
        solved, total = c["outcomes"]
        lo, hi = params.outcomes(c)
        print(f"  {key}  инцидентов {len(c['instances'])}; исходов {total:.1f}, модель решила сама {solved:.1f} — "
              f"надёжность [{lo:.2f}; {hi:.2f}]")
        print(f"    обобщение: {'; '.join(c['shape'])}")
        for rule, (k, n) in sorted(c["rules"].items()):
            lo, hi = params.rule(rule, c)
            print(f"    {rule:12} верно {k:.1f} из {n:.1f} на этой структуре — надёжность [{lo:.2f}; {hi:.2f}]")
    version = params.save(args.model, f"кластеры инцидентов {os.path.basename(args.clusters)}")
    print(f"Версия параметров {version} (откат — learn.py --rollback)")

    if args.pairs:
        out, mine = pairs(rows, args.replay, args.replay_share)
        with open(args.pairs, "w", encoding="utf-8") as f:
            for row in out:
                f.write(json.dumps(row, ensure_ascii=False) + "\n")
        print(f"Пар для дообучения в {args.pairs}: {len(out)} — из инцидентов {mine}, остальное — базовый корпус")


if __name__ == "__main__":
    main()
