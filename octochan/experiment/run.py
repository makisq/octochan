"""Стенд эксперимента 9.2 манифеста ФПм: прогоны задач в четырёх условиях.

Условия (раздел 9.2):

  natural        — 1: естественный язык;
  formal         — 2: формальный промпт с разбором, арбитрами и отчётом, без колец;
  protocol       — 3: то же с кольцами 1–3 в режиме протокола;
  architectural  — 4: своя модель ℒ, у которой логика и кольца в её коде.

В условиях 1–3 модель одна и та же — внешняя (протокольный подход); её ещё
не выбрали, и для них здесь только подготовка промптов: прогон отказывает
с причиной. Условие 4 запускается: ochan llm generate против адаптера
архитектурной модели.

Прогон — задача × запись × повтор. План ответа — пары «вопрос → ответ»
X(I) из отчёта (А.5): по ним metrics.py считает согласованность и
правильность.

    python run.py prepare --from "sources/*.fp" --lexicon ../fp/lexicon.yaml   # задачи из промптов и natural.yaml
    python run.py run --condition architectural --model own-small-2 --endpoint http://127.0.0.1:8093 \
        --ochan ochan.exe --home G:/fpm-llm/runs/exp-home --out runs/architectural-own-small-2.jsonl
"""

import argparse
import glob
import json
import os
import random
import re
import subprocess
import sys
import time

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "llama"))

import logic  # noqa: E402

CONDITIONS = ("natural", "formal", "protocol", "architectural")
SET = re.compile(r"^\s*set\s+(\w+)\.(\w+)\s*=\s*(.+?)\s*$")


def load_plan():
    with open(os.path.join(HERE, "plan.yaml"), encoding="utf-8") as f:
        return yaml.safe_load(f)


def load_tasks(pattern=None):
    tasks = []
    for path in sorted(glob.glob(pattern or os.path.join(HERE, "tasks", "*.yaml"))):
        with open(path, encoding="utf-8") as f:
            tasks.append(yaml.safe_load(f))
    return tasks


def plan_of(answer, task):
    """X(I): вопрос отчёта → выбор; и решения — правки новых ячеек ответа
    (цель.атрибут → значение), не ячеек задачи: по ним ищутся ошибки вне
    отчёта."""
    given = set(logic.parse(task).cells)
    report, decisions, q = {}, {}, None
    section = None
    for line in answer.splitlines():
        if line and not line[0].isspace():
            cell = re.match(r"cell\s+(\w+)", line)
            section = "report" if line.startswith("report:") else ("cell" if cell and cell.group(1) not in given else None)
            continue
        body = line.strip()
        if section == "report":
            key, _, value = body.partition(":")
            if key == "q":
                q = value.strip()
            elif key == "choice" and q:
                report[q] = value.strip()
        elif section == "cell" and (m := SET.match(line)):
            decisions[f"{m.group(1)}.{m.group(2)}"] = m.group(3)
    return report, decisions


# --- Подготовка задач --------------------------------------------------------

def weights(text):
    """w(q) = 1 + n(q): n — число отношений задачи, опирающихся на класс цели
    решения по вопросу q (А.5). Фиксируются до замера."""
    task = logic.parse(text)
    out = {}
    for d in logic.derive(task)[0]:
        n = sum(1 for frm, _, to, _ in task.relations if d.target in (frm, to))
        out[d.attr] = max(out.get(d.attr, 1), 1 + n)
    return out


def rewrite(text, lexicon, seed):
    """Другая формальная запись той же задачи: отношения и правки в другом
    порядке, слова классов — синонимы того же смысла."""
    rng = random.Random(seed)
    same = {}
    for word, entry in lexicon["words"].items():
        if len(entry["senses"]) == 1:
            same.setdefault(entry["senses"][0], []).append(word)
    out, block = [], []

    def flush():
        if block:
            rng.shuffle(block)
            out.extend(block)
            block.clear()

    goal = re.search(r"^goal:\s*(\w+)", text, re.M)
    section = None
    for line in text.splitlines():
        if line and not line[0].isspace():
            flush()
            section = line.split(":")[0].split()[0]
            out.append(line)
            continue
        m = re.match(r'^(\s+)(\w+)(\s*:\s*)"(\w+)"\s*$', line)
        # Слово цели называет goal: — его не меняют.
        if section == "classes" and m and not (goal and m.group(4) == goal.group(1)):
            options = [w for w in same.get(m.group(2), []) if w != m.group(4)]
            word = rng.choice(options) if options and rng.random() < 0.7 else m.group(4)
            out.append(f'{m.group(1)}{m.group(2)}{m.group(3)}"{word}"')
        elif section in ("relations", "cell") and line.strip():
            block.append(line)
        else:
            flush()
            out.append(line)
    flush()
    return "\n".join(out) + "\n"


def prepare(args):
    with open(args.lexicon, encoding="utf-8") as f:
        lexicon = yaml.safe_load(f)
    with open(os.path.join(HERE, "natural.yaml"), encoding="utf-8") as f:
        natural = yaml.safe_load(f)
    os.makedirs(os.path.join(HERE, "tasks"), exist_ok=True)
    for path in sorted(p for pattern in args.sources for p in glob.glob(pattern)):
        name = os.path.splitext(os.path.basename(path))[0]
        target = os.path.join(HERE, "tasks", f"{name}.yaml")
        if os.path.exists(target) and not args.force:
            print(f"{target}: уже есть (--force — перезаписать)")
            continue
        text = open(path, encoding="utf-8").read()
        plan = logic.AnswerPlan(text)
        about = natural.get(name, {})
        task = {
            "id": name,
            "domain": about.get("domain", ""),
            "status": "черновик: эталон и пересказы утверждает владелец",
            "intended": {d.attr: d.value for d in plan.decisions},
            "open": sorted({q.attr for q in plan.open} | {a for _, a in plan.task.inexact if not any(
                d.attr == a for d in plan.decisions)}),
            "weights": weights(text),
            "natural": about.get("natural", []),
            "formal": [text] + [rewrite(text, lexicon, f"{name}/{k}") for k in (1, 2)],
            "lacking": [text.rstrip("\n") + "\n\nrequires: incident_history\n"],
        }
        with open(target, "w", encoding="utf-8") as f:
            yaml.safe_dump(task, f, allow_unicode=True, sort_keys=False, width=100)
        print(f"{target}: записей {len(task['formal'])}, вопросов {len(task['intended'])}")


# --- Прогоны -----------------------------------------------------------------

def run_architectural(args, task, kind, k, text, env):
    path = os.path.join(args.home, "tasks", f"{task['id']}-{kind}{k}.fp")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(text)
    started = time.time()
    done = subprocess.run([args.ochan, "llm", "generate", "--prompt", path], capture_output=True, text=True,
                          encoding="utf-8", env=env)
    seconds = time.time() - started
    report, decisions = plan_of(done.stdout, text) if done.returncode == 0 else ({}, {})
    m = re.search(r"Логика:\s+утверждений (\d+) — .*?среди допустимых: (\d+)", done.stderr)
    return {"ok": done.returncode == 0, "refused": "не хватает предпосылок" in done.stderr,
            "plan": report, "decisions": decisions, "seconds": round(seconds, 2),
            "forced": int(m.group(1)) if m else 0, "chosen": int(m.group(2)) if m else 0,
            "reason": "" if done.returncode == 0 else done.stderr.strip()[-300:]}


def run(args):
    if args.condition != "architectural":
        raise SystemExit(f"условие {args.condition}: модель для условий 1–3 не выбрана (experiment/README.md, "
                         f"«Что ещё не запускается»); промпты готовы в задачах — natural и formal")
    plan = load_plan()
    os.makedirs(os.path.join(args.home, ".octochan"), exist_ok=True)
    config = os.path.join(args.home, "config.yaml")
    with open(config, "w", encoding="utf-8") as f:
        f.write(f"llm:\n  mode: embedded\n  endpoint: {args.endpoint}\n  timeout: 120s\n")
    env = dict(os.environ, HOME=args.home, USERPROFILE=args.home, OCTOCHAN_CONFIG=config)
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    with open(args.out, "w", encoding="utf-8") as out:
        for task in load_tasks(args.tasks):
            for kind in ("formal", "lacking"):
                for k, text in enumerate(task.get(kind, [])):
                    for repeat in range(plan["runs_per_writing"]):
                        row = {"task": task["id"], "kind": kind, "writing": k, "run": repeat,
                               "condition": args.condition, "model": args.model,
                               **run_architectural(args, task, kind, k, text, env)}
                        out.write(json.dumps(row, ensure_ascii=False) + "\n")
            print(f"{task['id']}: готово", flush=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("prepare", help="черновики задач из формальных промптов")
    p.add_argument("--from", dest="sources", nargs="+", required=True)
    p.add_argument("--lexicon", required=True)
    p.add_argument("--force", action="store_true")
    r = sub.add_parser("run", help="прогоны одного условия")
    r.add_argument("--condition", choices=CONDITIONS, required=True)
    r.add_argument("--model", required=True, help="метка модели: по ней H4 сравнивает версии")
    r.add_argument("--endpoint", default="http://127.0.0.1:8093")
    r.add_argument("--ochan", default="ochan")
    r.add_argument("--home", required=True, help="дом ochan для прогонов: свой журнал")
    r.add_argument("--tasks", help="шаблон задач (по умолчанию tasks/*.yaml)")
    r.add_argument("--out", required=True)
    args = ap.parse_args()
    {"prepare": prepare, "run": run}[args.cmd](args)


if __name__ == "__main__":
    main()
