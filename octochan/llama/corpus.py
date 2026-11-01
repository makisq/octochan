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

Обобщение — схема отношения (generalization с именем отношения): слот типа
источника, второй слот с доменом или диапазоном, операция скилла. Частный
случай (instance) — стрелка этой схемы: имя стрелки — ссылка, через которую
ячейки правят частный случай, значения слотов — имена частных случаев, у
которых нет своего определения. Вывод по стрелке:

- атрибут, который c1 задаёт обобщению, переходит на каждую его стрелку,
  если стрелке он не задан: set h1.x = v, basis: relation(имя обобщения).
  Атрибут, совпадающий со слотом, не переходит: значение слота задаёт сама
  стрелка;
- обратно не переходит ничего: атрибут одной стрелки не становится ни
  атрибутом обобщения, ни атрибутом соседних стрелок.

Знание задачи K — имена отношений, обобщения, частные случаи и операции из
provides: скиллов (import). requires:, действие жёсткого правила и
операция обобщения должны быть в K (или среди встроенных write, save,
exec), классы скилла — в classes:, стрелка — к объявленному обобщению и со
всеми слотами. Если предпосылки нет, это ⊥undef, и модель не решает за
оператора (H3): ответ — задача без единого решения, а находки по ответу
даёт проверка ochan. Такие задачи — близнецы решаемых: та же задача без
одной предпосылки, часто с именем, похожим на настоящее (fault_error
вместо fault_objective), — различающие примеры.

Пары синтетические: они учат языку, форме ответа и выводу по отношениям,
а не предметной области. Каждую проверяет разбор ochan (fpcheck): задача —
без отказов, ответ — строже, как текст модели; у близнеца без предпосылки
отказы — ровно ⊥undef на каждую недостающую. Непрошедшие отбрасываются.

    go build -o fpcheck.exe ./llama/fpcheck
    python corpus.py --lexicon ../fp/lexicon.yaml --fpcheck fpcheck.exe --count 5000 --out corpus.jsonl \
        --tasks G:/fpm-llm/runs/tasks-test
"""

import argparse
import collections
import copy
import json
import os
import random
import re
import subprocess

import yaml

import logic

# requires — тип отношения, но и раздел requires:; в корпус не берётся,
# чтобы модель не путала их.
RELATIONS = ["defines", "forces", "limits", "modifies", "precedes", "terminates"]
# Отношения, которые переносят атрибут источника на цель.
CARRIES = {"defines", "limits", "modifies"}
NUMBER_UNITS = ["MB", "GB", "s", "ms", "%", "m"]
# Действия среды выполнения: известны арбитру без скиллов (arbiter.Builtins).
BUILTINS = {"exec", "save", "write"}
VERSIONS = ["v1", "v2", "v3", "v1.2", "v2.3"]

COMMENT = re.compile(r"\s*#.*$")
# Недостающие предпосылки и вывод решений — логика модели, одна на корпус
# и модель.
missing = logic.missing


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


def particular(rng, used):
    """Имя частного случая: не слово языка и не составное из слов
    (plodu2222_ru). Определения у него нет — смысл даёт стрелка."""
    while True:
        stem = "".join(rng.choice("bdfgklmnprstvz") + rng.choice("aeiou") for _ in range(rng.randint(2, 3)))
        candidate = f"{stem}{rng.randint(1, 9999)}" + (f"_{rng.choice(['ru', 'io', 'loc', 'lan'])}"
                                                       if rng.random() < 0.4 else "")
        if candidate not in used:
            used.add(candidate)
            return candidate


def arrow(rng, used):
    """Имя стрелки — непрозрачная ссылка (h121151)."""
    while True:
        candidate = f"{rng.choice('hkmprtx')}{rng.randint(1000, 999999)}"
        if candidate not in used:
            used.add(candidate)
            return candidate


def value(rng, lex):
    if rng.random() < 0.25:
        return rng.choice(lex.words)
    n = rng.choice([1, 2, 3, 4, 5, 8, 10, 16, 20, 30, 32, 50, 64, 100, 128, 200, 256, 300, 400, 500, 1000])
    return f"{n} {rng.choice(lex.units)}" if rng.random() < 0.6 else str(n)


def vague(rng, lex, v, inside):
    """Неточное значение: варианты-слова или диапазон. v — значение
    источника: inside — оно лежит внутри, иначе — вне; None — источника нет."""
    m = logic.NUMBER.match(v) if v else None
    if m or (v is None and rng.random() < 0.4):
        n = int(float(m.group(1))) if m else rng.choice([10, 50, 100, 200])
        unit = (m.group(2) or "") if m else rng.choice(["", rng.choice(lex.units)])
        lo, hi = (max(0, n // 2), n * 2 + 1) if inside else (n + 1, n * 3 + 2)
        return f"{lo}..{hi}" + (f" {unit}" if unit else "")
    others = [w for w in rng.sample(lex.words, 4) if w != v][:rng.randint(1, 3)]
    variants = ([v] if inside else []) + others
    if len(variants) < 2:
        variants.append(next(w for w in lex.words if w not in variants and w != v))
    rng.shuffle(variants)
    return "{" + ", ".join(variants) + "}"


def task(rng, lex):
    """Случайная задача на ℒ — структура для render и lacking."""
    used, attrs = set(), set()
    classes = rng.sample(lex.classes, rng.randint(3, 5))
    words = {c: rng.choice(lex.single[c]) for c in classes}
    goal = classes[-1]

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
    relations = [(a, t, b, None if t == "strives_to" else name(rng, lex, used)) for a, t, b in relations]

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
        carriers = [a for a, t, _, _ in relations if t in CARRIES]
        target = rng.choice(carriers) if carriers and rng.random() < 0.7 else rng.choice(classes[:-1])
        sets.append((target, name(rng, lex, attrs, parts=1), value(rng, lex)))

    # Неточная предпосылка (раздел 5.2) — чаще на цели переноса: значение
    # источника внутри неё её сужает, вне — вопрос оператору; без источника
    # её сужает только история.
    if rng.random() < 0.3:
        given = {(t, a) for t, a, _ in sets}
        carried = [(b, a, v) for frm, kind, b, label in relations if kind in CARRIES and label
                   for s, a, v in sets if s == frm and (b, a) not in given]
        if carried and rng.random() < 0.8:
            target, attr, v = rng.choice(carried)
            sets.append((target, attr, vague(rng, lex, v, rng.random() < 0.7)))
        else:
            sets.append((rng.choice(classes[:-1]), name(rng, lex, attrs, parts=1), vague(rng, lex, None, False)))

    # Обобщение — схема одного из отношений, частные случаи — её стрелки.
    gen = None
    labeled = [(a, b, r) for a, _, b, r in relations if r]
    if labeled and rng.random() < 0.5:
        source, result, label = rng.choice(labeled)
        slots = set()
        first, second = name(rng, lex, slots, parts=1), name(rng, lex, slots, parts=1)
        if rng.random() < 0.5:
            spec = sorted(rng.sample(lex.words, rng.randint(2, 4)))
        else:
            lo = rng.choice([1, 2, 5, 10, 20, 50, 100])
            spec = (lo, lo * rng.choice([2, 4, 5, 10]))
        gen = {"name": label, "source": source, "result": result, "slots": [first, second], "spec": spec,
               "op": name(rng, lex, used), "instances": []}
        for _ in range(rng.choice([1, 1, 2, 2, 3])):
            v = rng.choice(spec) if isinstance(spec, list) else str(rng.randint(*spec))
            gen["instances"].append({"name": arrow(rng, used), "of": label,
                                     "values": {first: particular(rng, used), second: v}})
        arrows = [i["name"] for i in gen["instances"]]
        if rng.random() < 0.75:
            attr = name(rng, lex, attrs, parts=1)
            sets.append((label, attr, value(rng, lex)))
            if rng.random() < 0.3:
                # Своё значение у стрелки: точная посылка о частном случае.
                sets.append((rng.choice(arrows), attr, value(rng, lex)))
        if rng.random() < 0.3:
            # Атрибут стрелки не переходит ни на обобщение, ни на соседей.
            sets.append((rng.choice(arrows), name(rng, lex, attrs, parts=1), value(rng, lex)))

    # Знание задачи: скилл, требования, правило о его операции.
    # Операцию обобщения даёт скилл (П5): с обобщением скилл есть всегда.
    skill, own = None, rng.random() < 0.45
    if own or gen:
        skill = {"name": name(rng, lex, used, parts=1), "version": rng.choice(VERSIONS),
                 "classes": rng.sample(classes, rng.randint(1, 2)),
                 "ops": [name(rng, lex, used) for _ in range(rng.randint(1, 2) if own else 0)],
                 "needs": [rng.choice(classes)] if rng.random() < 0.5 else []}
        if gen:
            skill["ops"].append(gen["op"])
    known = [r for *_, r in relations if r] + (skill["ops"] if skill else [])
    known += [i["name"] for i in gen["instances"]] if gen else []
    requires = rng.sample(known, min(len(known), rng.randint(1, 2))) if rng.random() < 0.45 else []
    rule = None
    if rng.random() < 0.4:
        action = rng.choice(skill["ops"]) if skill and rng.random() < 0.6 else rng.choice(["write", "save"])
        rule = (action, name(rng, lex, used))
    return {"classes": classes, "words": words, "goal": goal, "relations": relations, "priority": priority,
            "gen": gen, "skill": skill, "requires": requires, "rule": rule, "sets": sets}


def render(t):
    """Текст задачи."""
    classes, width = t["classes"], max(len(c) for c in t["classes"])
    lines = [f"goal: {t['words'][t['goal']]}", "", "classes:"]
    lines += [f"  {c.ljust(width)} : \"{t['words'][c]}\"" for c in classes]
    lines += ["", "relations:"]
    for a, kind, b, rel in t["relations"]:
        if rel is None:
            lines.append(f"  {a.ljust(width)} --{kind}--> {b}")
        else:
            lines.append(f"  {a.ljust(width)} --{kind}--> {b.ljust(width)} ({rel})")
    if t["priority"]:
        lines += ["", "priority:", f"  {t['priority'][0]} < {t['priority'][1]}"]
    gen = t.get("gen")
    if gen:
        a, b = gen["slots"]
        spec = gen["spec"]
        spec = f"{{{', '.join(spec)}}}" if isinstance(spec, list) else f"{spec[0]}..{spec[1]}"
        lines += ["", f"generalization {gen['name']}({a} : {gen['source']}, {b} : {spec}) :",
                  f"  {gen['result']} = {gen['op']}({a}, {b})", ""]
        for i in gen["instances"]:
            bound = ", ".join(f"{s} := {v}" for s, v in i["values"].items())
            lines.append(f"instance {i['name']} = {i['of']}({bound})")
    skill = t["skill"]
    if skill:
        lines += ["", f"import @{skill['name']} {skill['version']}",
                  f"  provides: {', '.join(skill['classes'] + skill['ops'])}"]
        if skill["needs"]:
            lines.append(f"  requires_class: {', '.join(skill['needs'])}")
    if t["requires"]:
        lines += ["", f"requires: {', '.join(t['requires'])}"]
    if t["rule"]:
        lines += ["", f"hard forbid {t['rule'][0]}(except = {{{t['rule'][1]}}})"]
    lines += ["", "cell c1:"]
    lines += [f"  set {c}.{a} = {v}" for c, a, v in t["sets"]]
    return "\n".join(lines) + "\n"


def absent(rng, lex, known, like=None):
    """Имя, которого нет в K: чаще — настоящее (like, иначе любое из K) с
    одним словом не тем (fault_error вместо fault_objective), иначе — новое."""
    compounds = [like] if like else [k for k in known if "_" in k]
    for _ in range(100):
        if compounds and rng.random() < 0.6:
            parts = rng.choice(compounds).split("_")
            parts[rng.randrange(len(parts))] = rng.choice(lex.words)
            candidate = "_".join(parts)
        else:
            candidate = "_".join(rng.sample(lex.words, 2))
        if candidate not in known and candidate not in BUILTINS:
            return candidate
    raise RuntimeError("не нашлось имени вне K")


def lacking(rng, lex, t):
    """Близнец задачи без одной предпосылки (H3): требование или действие
    правила вне K, класс скилла не из classes:, стрелка к необъявленному
    обобщению или с незаполненным слотом, операция обобщения не из скилла."""
    t = copy.deepcopy(t)
    gen = t.get("gen")
    known = {r for *_, r in t["relations"] if r} | set(t["skill"]["ops"] if t["skill"] else [])
    known |= {i["name"] for i in gen["instances"]} if gen else set()
    kind = rng.choice(["requires", "rule"] + (["class"] if t["skill"] else []) + (["gen", "slot", "op"] if gen else []))
    if kind == "gen":
        rng.choice(gen["instances"])["of"] = absent(rng, lex, known, like=gen["name"])
    elif kind == "slot":
        values = rng.choice(gen["instances"])["values"]
        del values[rng.choice(sorted(values))]
    elif kind == "op":
        gen["op"] = absent(rng, lex, known | {gen["op"]})
    elif kind == "class":
        other = rng.choice([c for c in lex.classes if c not in t["classes"]])
        if rng.random() < 0.5:
            t["skill"]["needs"] = [other]
        else:
            t["skill"]["classes"].append(other)
    elif kind == "rule":
        t["rule"] = (absent(rng, lex, known), t["rule"][1] if t["rule"] else name(rng, lex, set(known)))
    else:
        requires = t["requires"] or [None]
        requires[rng.randrange(len(requires))] = absent(rng, lex, known)
        t["requires"] = requires
    return t


def copy_of(text):
    """Задача, как её повторяет ответ: без комментариев и лишних пустых строк."""
    copy = "\n".join(COMMENT.sub("", line).rstrip() for line in text.splitlines()).strip("\n") + "\n"
    return re.sub(r"\n{3,}", "\n\n", copy)


def solve(text):
    """Ответ модели на задачу: задача без комментариев, решения и отчёт —
    решения выводит логика модели (logic.py; та же, что у ochan). Без
    предпосылки (missing) решений нет: модель не решает за оператора. Два
    значения одного вопроса отчёта одной записью не сказать — такой задаче
    ответа нет (None)."""
    copy = copy_of(text)
    if missing(text):
        return copy
    plan = logic.AnswerPlan(text)
    if any(len(q.targets) > 1 for q in plan.open):
        return None
    return copy.rstrip("\n") + plan.text()[len(plan.copy) - 1:]


def kind(task, answer):
    """Вид пары для сводки."""
    if missing(task):
        return "без предпосылки"
    parsed = logic.parse(task)
    made_on = set(re.findall(r"set (\w+\.\w+)", answer[len(copy_of(task)):]))
    vague = {f"{t}.{a}" for t, a in parsed.inexact}
    if vague:
        return "неточное: сужено" if made_on & vague else "неточное: вопрос"
    if "priority(" in answer:
        return "конфликт"
    arrows = {name for name, _, _ in parsed.instances}
    made = set(re.findall(r"set (\w+)\.", answer[len(copy_of(task)):]))
    if made & arrows:
        return "по стрелке" if made <= arrows else "перенос и по стрелке"
    return "перенос" if made else "без решений"


def check(fpcheck, texts):
    """Вердикты fpcheck по текстам [(id, text, model)]."""
    feed = "".join(json.dumps({"id": i, "text": t, "model": m}) + "\n" for i, t, m in texts)
    out = subprocess.run([fpcheck], input=feed.encode(), capture_output=True, check=True)
    return {v["id"]: v for v in map(json.loads, out.stdout.decode().splitlines())}


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--lexicon", required=True, help="fp/lexicon.yaml")
    ap.add_argument("--fpcheck", required=True)
    ap.add_argument("--count", type=int, default=5000)
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--test", type=float, default=0.1, help="доля отложенных пар")
    ap.add_argument("--h3", type=float, default=0.5,
                    help="доля задач с решениями, у которых есть близнец без предпосылки (у задач без решений — треть её)")
    ap.add_argument("--tasks", help="каталог: первые --tasks-count отложенных задач файлами .fp для замера")
    ap.add_argument("--tasks-count", type=int, default=40)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    rng = random.Random(args.seed)
    lex = Lexicon(args.lexicon)
    # Группа — задача и её близнец без предпосылки: группа целиком либо в
    # обучении, либо в отложенных.
    groups, seen, total = [], set(), 0
    while total < int(args.count * 1.3):
        base = task(rng, lex)
        texts = [render(base)]
        decided = solve(texts[0]) not in (None, copy_of(texts[0]))
        if rng.random() < (args.h3 if decided else args.h3 / 3):
            texts.append(render(lacking(rng, lex, base)))
        pairs = [(t, solve(t)) for t in texts]
        if all(a and t not in seen for t, a in pairs):  # отложенная задача не встречается в обучении
            seen.update(texts)
            groups.append(pairs)
            total += len(pairs)
    flat = [(g, k, t, a) for g, pairs in enumerate(groups) for k, (t, a) in enumerate(pairs)]
    verdicts = check(args.fpcheck, [(f"t{g}.{k}", t, False) for g, k, t, _ in flat] +
                     [(f"a{g}.{k}", a, True) for g, k, _, a in flat])

    def accepted(g, k, t):
        found = (verdicts[f"t{g}.{k}"], verdicts[f"a{g}.{k}"])
        lack = missing(t)
        if not lack:
            return all(v["ok"] for v in found), found
        want = f"⊥undef {len(lack)}"
        return all(v["syntax"] and v["codes"] == want for v in found), found

    kept, failures = [], collections.Counter()
    for g, pairs in enumerate(groups):
        group = []
        for k, (t, answer) in enumerate(pairs):
            ok, found = accepted(g, k, t)
            if not ok:
                first = next((v["first"] for v in found if not v["ok"]), ": : ожидался ⊥undef, а отказов нет")
                failures[first.split(": ", 2)[-1][:90]] += 1
                continue
            group.append({"task": t, "answer": answer})
        if group:
            kept.append(group)
        if sum(map(len, kept)) >= args.count:
            break

    rows, split = [], int(sum(map(len, kept)) * (1 - args.test))
    for group in kept:
        part = "train" if len(rows) < split else "test"
        rows += [{**row, "split": part} for row in group]
    with open(args.out, "w", encoding="utf-8") as f:
        for row in rows:
            f.write(json.dumps(row, ensure_ascii=False) + "\n")
    kinds = collections.Counter(kind(r["task"], r["answer"]) for r in rows)
    learned = sum(r["split"] == "train" for r in rows)
    print(f"Пар: {len(rows)} из {total} прошли проверку (обучение {learned}, отложено {len(rows) - learned}); "
          + ", ".join(f"{k} {n}" for k, n in kinds.most_common()))
    knowledge = collections.Counter(kind for r in rows for kind, mark in
                                    (("import", "\nimport @"), ("requires:", "\nrequires:"), ("правило", "\nhard "),
                                     ("обобщение", "\ngeneralization ")) if mark in r["task"])
    print("Знание в задачах: " + ", ".join(f"{k} {n}" for k, n in knowledge.most_common()))
    for reason, n in failures.most_common(5):
        print(f"  отброшено {n}: {reason}")
    if args.tasks:
        os.makedirs(args.tasks, exist_ok=True)
        for n, row in enumerate([r for r in rows if r["split"] == "test"][:args.tasks_count], 1):
            with open(os.path.join(args.tasks, f"test{n:02}.fp"), "w", encoding="utf-8", newline="\n") as f:
                f.write(row["task"])
        print(f"Задачи для замера: {args.tasks}")


if __name__ == "__main__":
    main()
