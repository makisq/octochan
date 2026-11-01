"""Корпус ℒ для дообучения своей модели — шаг 3.

Пара — задача оператора и ответ модели, оба на формальном промпте:

- задача: цель, классы (смысл слова из словаря), отношения из словаря R с
  именами из слов, иногда приоритет и жёсткое правило, ячейка c1;
- ответ: та же программа, закрытая там, где задача оставила открытым:
  ячейка с решениями и запись report: на каждое решение.

Ответ — функция задачи (solve): кольцо 0 берёт токен только с p ≥ τ, и
ответ проходит его, только если задача его определяет. Решение выводится
из задачи, а не угадывается:

- отношение defines, limits или modifies переносит атрибут, который c1
  задаёт источнику, на цель, если цели он не задан: set B.x = v,
  basis: relation(имя);
- в report: вопрос — параметр (q: x), он покрывает все цели с тем же
  значением;
- два источника дают цели разные значения — конфликт; его решает
  приоритет задачи (A < B: побеждает A): выбор — значение победителя,
  альтернативы — значения источников, basis: priority(A < B). Без
  приоритета решения нет: модель не выбирает за оператора;
- значение, заданное задачей, не переписывается; решений нет — ответ равен
  задаче.

Пары синтетические: они учат языку, форме ответа и выводу по отношениям,
а не предметной области. Каждую проверяет разбор ochan (fpcheck): задача —
без отказов, ответ — строже, как текст модели. Непрошедшие отбрасываются.

    go build -o fpcheck.exe ./llama/fpcheck
    python corpus.py --lexicon ../fp/lexicon.yaml --fpcheck fpcheck.exe --count 4000 --out corpus.jsonl \
        --tasks G:/fpm-llm/runs/tasks-test
"""

import argparse
import collections
import json
import os
import random
import re
import subprocess

import yaml

# requires — тип отношения, но и раздел requires:; в корпус не берётся,
# чтобы модель не путала их.
RELATIONS = ["defines", "forces", "limits", "modifies", "precedes", "terminates"]
# Отношения, которые переносят атрибут источника на цель.
CARRIES = {"defines", "limits", "modifies"}
NUMBER_UNITS = ["MB", "GB", "s", "ms", "%", "m"]

COMMENT = re.compile(r"\s*#.*$")
RELATION = re.compile(r"(\w+)\s+--(\w+)-->\s+(\w+)\s*(?:\((\w+)\))?$")
SET = re.compile(r"set\s+(\w+)\.(\w+)\s*=\s*(.+?)\s*$")
CELL = re.compile(r"cell\s+(\w+)\s*:")


class Lexicon:
    def __init__(self, path):
        with open(path, encoding="utf-8") as f:
            data = yaml.safe_load(f)
        self.words = sorted(data["words"])
        # Класс задаётся словом с одним смыслом: тогда ⊥ambig не бывает.
        self.single = collections.defaultdict(list)
        for word, entry in data["words"].items():
            if len(entry["senses"]) == 1:
                self.single[entry["senses"][0]].append(word)
        self.classes = sorted(c for c, words in self.single.items() if words)
        self.units = [u for u in data.get("units", []) if u in NUMBER_UNITS] or NUMBER_UNITS


def name(rng, lex, used, parts=2):
    """Имя из слов словаря через _, ещё не занятое."""
    for _ in range(100):
        candidate = "_".join(rng.sample(lex.words, parts))
        if candidate not in used:
            used.add(candidate)
            return candidate
    raise RuntimeError("не нашлось свободного имени")


def value(rng, lex):
    if rng.random() < 0.25:
        return rng.choice(lex.words)
    n = rng.choice([1, 2, 3, 4, 5, 8, 10, 16, 20, 30, 32, 50, 64, 100, 128, 200, 256, 300, 400, 500, 1000])
    return f"{n} {rng.choice(lex.units)}" if rng.random() < 0.6 else str(n)


def task(rng, lex):
    """Случайная задача на ℒ."""
    used, attrs = set(), set()
    classes = rng.sample(lex.classes, rng.randint(3, 5))
    words = {c: rng.choice(lex.single[c]) for c in classes}
    goal = classes[-1]
    width = max(len(c) for c in classes)

    relations = []
    for a, b in zip(classes, classes[1:]):
        relations.append((a, "strives_to" if b == goal else rng.choice(RELATIONS), b))
    # Второй источник у той же цели: из него бывает конфликт.
    conflict = None
    if len(classes) >= 4 and rng.random() < 0.5:
        target = rng.choice(classes[2:-1])
        source = rng.choice(classes[:classes.index(target) - 1])
        relations.append((source, rng.choice(sorted(CARRIES)), target))
        before = next(a for a, _, b in relations if b == target)
        if rng.random() < 0.7 and next(t for a, t, b in relations if b == target) in CARRIES:
            conflict = (before, source)

    priority = None
    if conflict and rng.random() < 0.85:
        priority = list(conflict)
        rng.shuffle(priority)
    elif rng.random() < 0.4:
        priority = rng.sample(classes[:-1], 2)

    sets = []
    if conflict:
        # Оба источника задают один атрибут, разными именами.
        attr = name(rng, lex, attrs, parts=1)
        first, second = rng.sample(lex.words, 2)
        sets += [(conflict[0], attr, first), (conflict[1], attr, second)]
    for _ in range(rng.randint(1, 2)):
        # Чаще — источнику отношения, которое переносит атрибут.
        carriers = [a for a, t, _ in relations if t in CARRIES]
        target = rng.choice(carriers) if carriers and rng.random() < 0.7 else rng.choice(classes[:-1])
        sets.append((target, name(rng, lex, attrs, parts=1), value(rng, lex)))

    lines = [f"goal: {words[goal]}", "", "classes:"]
    lines += [f"  {c.ljust(width)} : \"{words[c]}\"" for c in classes]
    lines += ["", "relations:"]
    for a, t, b in relations:
        if t == "strives_to":
            lines.append(f"  {a.ljust(width)} --strives_to--> {b}")
        else:
            lines.append(f"  {a.ljust(width)} --{t}--> {b.ljust(width)} ({name(rng, lex, used)})")
    if priority:
        lines += ["", "priority:", f"  {priority[0]} < {priority[1]}"]
    if rng.random() < 0.4:
        lines += ["", f"hard forbid {rng.choice(['write', 'save'])}(except = {{{name(rng, lex, used)}}})"]
    lines += ["", "cell c1:"]
    lines += [f"  set {c}.{a} = {v}" for c, a, v in sets]
    return "\n".join(lines) + "\n"


def parse(text):
    """Из задачи — то, из чего выводятся решения: отношения, приоритеты, ячейки."""
    relations, chains, given, cells, section = [], [], {}, set(), None
    for line in text.splitlines():
        line = COMMENT.sub("", line).rstrip()
        if not line:
            continue
        if not line[0].isspace():
            section = line.split(":")[0].strip()
            m = CELL.match(line)
            if m:
                cells.add(m.group(1))
                section = "cell"
            continue
        body = line.strip()
        if section == "relations":
            m = RELATION.match(body)
            if m:
                relations.append(m.groups())
        elif section == "priority":
            chains.append([c.strip() for c in body.split("<")])
        elif section == "cell":
            m = SET.match(body)
            if m:
                given.setdefault((m.group(1), m.group(2)), m.group(3))
    return relations, chains, given, cells


def beats(chains, a, b):
    """a < b в приоритетах задачи: в конфликте побеждает a."""
    return any(a in chain and b in chain and chain.index(a) < chain.index(b) for chain in chains)


def solve(text):
    """Ответ модели на задачу: задача без комментариев, решения и отчёт."""
    relations, chains, given, cells = parse(text)
    copy = "\n".join(COMMENT.sub("", line).rstrip() for line in text.splitlines()).strip("\n") + "\n"
    copy = re.sub(r"\n{3,}", "\n\n", copy)

    # Что переносится: (цель, атрибут) -> [(источник, значение, имя отношения)].
    carried = collections.OrderedDict()
    for (source, attr), v in given.items():
        for a, t, b, rel in relations:
            if a == source and t in CARRIES and rel and (b, attr) not in given:
                carried.setdefault((b, attr), []).append((source, v, rel))

    decisions = []
    for (target, attr), sources in carried.items():
        values = list(dict.fromkeys(v for _, v, _ in sources))
        if len(values) == 1:
            decisions.append((target, attr, values[0], None, f"relation({sources[0][2]})"))
            continue
        winners = [s for s, v, _ in sources if all(beats(chains, s, o) for o, w, _ in sources if w != v)]
        if not winners:
            continue  # конфликт без приоритета: решает оператор
        win = winners[0]
        lose = next(o for o, w, _ in sources if w != given[(win, attr)])
        alternatives = values if all(re.fullmatch(r"[A-Za-z_]\w*", v) for v in values) else None
        decisions.append((target, attr, given[(win, attr)], alternatives, f"priority({win} < {lose})"))

    if not decisions:
        return copy
    # Вопрос — параметр (со строчной: через точку пишется объект плана, а не
    # класс); он покрывает параметр во всех целях с тем же значением. Один
    # параметр с разными значениями в отчёт не записать — такой задаче
    # ответа здесь нет.
    report = collections.OrderedDict()
    for t, a, v, alternatives, basis in decisions:
        if a in report and report[a][0] != v:
            return None
        report.setdefault(a, (v, alternatives, basis))
    cell = next(f"c{k}" for k in range(1, 100) if f"c{k}" not in cells)
    out = [copy.rstrip("\n"), "", f"cell {cell}:"]
    out += [f"  set {t}.{a} = {v}" for t, a, v, _, _ in decisions]
    out += ["", "report:"]
    for a, (v, alternatives, basis) in report.items():
        out += [f"  q: {a}", f"    choice: {v}"]
        if alternatives:
            out.append(f"    alternatives: {{{', '.join(alternatives)}}}")
        out.append(f"    basis: {basis}")
    return "\n".join(out) + "\n"


def check(fpcheck, texts):
    """Вердикты fpcheck по текстам [(id, text, model)]."""
    feed = "".join(json.dumps({"id": i, "text": t, "model": m}) + "\n" for i, t, m in texts)
    out = subprocess.run([fpcheck], input=feed.encode(), capture_output=True, check=True)
    return {v["id"]: v for v in map(json.loads, out.stdout.decode().splitlines())}


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--lexicon", required=True, help="fp/lexicon.yaml")
    ap.add_argument("--fpcheck", required=True)
    ap.add_argument("--count", type=int, default=4000)
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--test", type=float, default=0.1, help="доля отложенных пар")
    ap.add_argument("--tasks", help="каталог: первые --tasks-count отложенных задач файлами .fp для замера")
    ap.add_argument("--tasks-count", type=int, default=30)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    rng = random.Random(args.seed)
    lex = Lexicon(args.lexicon)
    pairs, seen = [], set()
    while len(pairs) < int(args.count * 1.3):
        t = task(rng, lex)
        answer = solve(t)
        if answer and t not in seen:  # отложенная задача не встречается в обучении
            seen.add(t)
            pairs.append((t, answer))
    verdicts = check(args.fpcheck, [(f"t{i}", t, False) for i, (t, _) in enumerate(pairs)] +
                     [(f"a{i}", a, True) for i, (_, a) in enumerate(pairs)])

    kept, failures = [], collections.Counter()
    for i, (t, answer) in enumerate(pairs):
        bad = [v for v in (verdicts[f"t{i}"], verdicts[f"a{i}"]) if not v["ok"]]
        if bad:
            failures[bad[0]["first"].split(": ", 2)[-1][:90]] += 1
            continue
        kept.append({"task": t, "answer": answer})
        if len(kept) == args.count:
            break

    split = int(len(kept) * (1 - args.test))
    with open(args.out, "w", encoding="utf-8") as f:
        for n, row in enumerate(kept):
            f.write(json.dumps({**row, "split": "train" if n < split else "test"}, ensure_ascii=False) + "\n")
    kinds = collections.Counter("priority(" in r["answer"] and "конфликт" or
                                "report:" in r["answer"] and "перенос" or "без решений" for r in kept)
    print(f"Пар: {len(kept)} из {len(pairs)} прошли проверку (обучение {split}, отложено {len(kept) - split}); "
          + ", ".join(f"{k} {n}" for k, n in kinds.most_common()))
    for reason, n in failures.most_common(5):
        print(f"  отброшено {n}: {reason}")
    if args.tasks:
        os.makedirs(args.tasks, exist_ok=True)
        for n, row in enumerate(kept[split:split + args.tasks_count], 1):
            with open(os.path.join(args.tasks, f"test{n:02}.fp"), "w", encoding="utf-8", newline="\n") as f:
                f.write(row["task"])
        print(f"Задачи для замера: {args.tasks}")


if __name__ == "__main__":
    main()
