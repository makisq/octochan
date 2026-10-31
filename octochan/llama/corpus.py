"""Корпус ℒ для дообучения своей модели — шаг 3.

Пара — задача оператора и ответ модели, оба на формальном промпте:

- задача: цель, классы (смысл слова из словаря), отношения из словаря R с
  именами из слов, иногда приоритет и жёсткое правило, ячейка c1;
- ответ: та же программа, закрытая там, где задача оставила открытым:
  ячейка с решениями модели и запись report: на каждое решение — выбор,
  альтернативы, основание (not_in_prompt или приоритет задачи).

Пары синтетические: они учат языку и форме ответа, а не предметной
области. Каждую проверяет разбор ochan (fpcheck): задача — без отказов,
ответ — строже, как текст модели. Непрошедшие отбрасываются.

    go build -o fpcheck.exe ./llama/fpcheck
    python corpus.py --lexicon ../fp/lexicon.yaml --fpcheck fpcheck.exe --count 4000 --out corpus.jsonl
"""

import argparse
import collections
import json
import random
import subprocess

import yaml

# requires — тип отношения, но и раздел requires:; в корпус не берётся,
# чтобы модель не путала их.
RELATIONS = ["defines", "forces", "limits", "modifies", "precedes", "terminates"]
NUMBER_UNITS = ["MB", "GB", "s", "ms", "%", "m"]


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
    n = rng.choice([1, 2, 3, 4, 5, 8, 10, 16, 20, 30, 32, 50, 64, 100, 128, 200, 256, 300, 400, 500, 1000])
    return f"{n} {rng.choice(lex.units)}" if rng.random() < 0.6 else str(n)


def pair(rng, lex):
    used = set()
    classes = rng.sample(lex.classes, rng.randint(3, 5))
    words = {c: rng.choice(lex.single[c]) for c in classes}
    goal = classes[-1]
    width = max(len(c) for c in classes)

    lines = [f"goal: {words[goal]}", "", "classes:"]
    lines += [f"  {c.ljust(width)} : \"{words[c]}\"" for c in classes]
    lines += ["", "relations:"]
    for a, b in zip(classes, classes[1:]):
        if b == goal:
            lines.append(f"  {a.ljust(width)} --strives_to--> {b}")
        else:
            lines.append(f"  {a.ljust(width)} --{rng.choice(RELATIONS)}--> {b.ljust(width)} ({name(rng, lex, used)})")

    priority = None
    if rng.random() < 0.5:
        priority = rng.sample(classes[:-1], 2)
        lines += ["", "priority:", f"  {priority[0]} < {priority[1]}"]
    if rng.random() < 0.4:
        lines += ["", f"hard forbid {rng.choice(['write', 'save'])}(except = {{{name(rng, lex, used)}}})"]

    attrs = set()
    lines += ["", "cell c1:"]
    for _ in range(rng.randint(1, 2)):
        target = rng.choice(classes)
        attr = name(rng, lex, attrs, parts=1)
        lines.append(f"  set {target}.{attr} = {value(rng, lex)}")
    task = "\n".join(lines) + "\n"

    # Ответ: задача и то, что она оставила открытым, — решения и отчёт.
    answer = lines + ["", "cell c2:"]
    report = ["", "report:"]
    for _ in range(rng.randint(1, 2)):
        target = rng.choice(classes)
        attr = name(rng, lex, attrs, parts=1)
        # Альтернативы — имена; у числа их не пишут.
        if rng.random() < 0.5:
            choice, other = rng.sample(lex.words, 2)
            alternatives = [f"    alternatives: {{{choice}, {other}}}"]
        else:
            choice, alternatives = rng.choice([1, 2, 4, 5, 8, 10, 16, 20, 50, 100, 200, 300, 500]), []
        answer.append(f"  set {target}.{attr} = {choice}")
        basis = f"priority({priority[0]} < {priority[1]})" if priority and rng.random() < 0.4 else "not_in_prompt"
        report += [f"  q: {attr}", f"    choice: {choice}", *alternatives, f"    basis: {basis}"]
    return task, "\n".join(answer + report) + "\n"


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
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    rng = random.Random(args.seed)
    lex = Lexicon(args.lexicon)
    pairs = [pair(rng, lex) for _ in range(int(args.count * 1.3))]
    verdicts = check(args.fpcheck, [(f"t{i}", t, False) for i, (t, _) in enumerate(pairs)] +
                     [(f"a{i}", a, True) for i, (_, a) in enumerate(pairs)])

    kept, failures = [], collections.Counter()
    for i, (task, answer) in enumerate(pairs):
        bad = [v for v in (verdicts[f"t{i}"], verdicts[f"a{i}"]) if not v["ok"]]
        if bad:
            failures[bad[0]["first"].split(": ", 2)[-1][:90]] += 1
            continue
        kept.append({"task": task, "answer": answer})
        if len(kept) == args.count:
            break

    split = int(len(kept) * (1 - args.test))
    with open(args.out, "w", encoding="utf-8") as f:
        for n, row in enumerate(kept):
            f.write(json.dumps({**row, "split": "train" if n < split else "test"}, ensure_ascii=False) + "\n")
    print(f"Пар: {len(kept)} из {len(pairs)} прошли проверку (обучение {split}, отложено {len(kept) - split})")
    for reason, n in failures.most_common(5):
        print(f"  отброшено {n}: {reason}")


if __name__ == "__main__":
    main()
