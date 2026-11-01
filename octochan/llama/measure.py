"""Замер шага: сырой вывод модели против встроенного режима ochan.

По каждой задаче (формальному промпту оператора):

- raw — модель пишет ответ сама, жадно, без колец и маски; ответ
  проверяется так же, как декодер принимает готовый текст:
  ochan fp check --model;
- embedded — ответ ведёт ochan (llm generate) через адаптер по протоколу
  v1: кольцо 0, маска языка, откаты. Из сводки колец берутся откаты,
  утверждения вне ℒ, отклонённые кольцом 0, позиции без токена.

Разбор ochan проверяет язык, но не то, сходится ли ответ с задачей. Это
отдельные метрики, у raw и у embedded:

- consistent — решения модели (set в ячейках, которых нет в задаче) не
  переписывают значения задачи, и в report: каждому решению — запись
  q: атрибут с тем же choice, без лишних записей;
- exact — ответ равен выводу из задачи (corpus.solve): копия задачи,
  решения по отношениям, приоритетам и стрелкам, отчёт. Сравниваются
  канонические записи (fpmtok.canonical): выравнивание пробелами и
  комментарии — не ответ.

Язык — отсутствие ⊥syn: ⊥undef в ответе — о задаче, а не о языке, и у
задачи без предпосылки (corpus.missing) он и должен быть. По таким задачам
— H3: модель не решает за оператора (в ответе ни одного решения), а по
решаемым — ложный отказ: решения нужны, а их нет.

Задачу без предпосылки ochan модели не передаёт и сам возвращает ⊥undef
(llm generate): embedded на ней — отказ системы, а не ответ модели. Копию
задачи в embedded пишет ochan, выводимое из посылок — логика (llm.derive);
модель выбирает только среди допустимых (forced — утверждений логики,
chosen — выборов модели). raw — что модель сделала бы сама, целиком.

ochan запускается в отдельном доме (--home): журнал и роли пользователя
замер не трогает. Скиллы задачи ставятся в этот дом перед ней пакетами-
заглушками: операции — ровно из provides:, как объявила задача. Проверка
ochan смотрит в библиотеку, как в настоящей работе, а синтетический скилл
так становится установленным. Адаптер должен быть запущен (adapter.py serve).

    python measure.py --model HuggingFaceTB/SmolLM2-135M-Instruct --ochan ochan.exe \
        --endpoint http://127.0.0.1:8090 --home G:/fpm-llm/runs/home --out run.jsonl tasks/*.fp
"""

import argparse
import glob
import json
import os
import re
import shutil
import subprocess
import time

from adapter import TASK_MARKER, engine_for
from corpus import copy_of, missing, solve
from fpmtok import canonical

SUMMARY = {
    "statements": r"Утверждений: (\d+), токенов: (\d+), обращений к модели: (\d+)",
    "ring0": r"Кольцо 0:\s+отклонено (\d+)",
    "language": r"утверждений вне ℒ (\d+), токенов вне языка (\d+)",
    "rollbacks": r"Откатов:\s+(\d+) к началу утверждения; позиций без токена выше порога: (\d+)",
    "logic": r"Логика:\s+утверждений (\d+) — единственные допустимые; выбрано моделью среди допустимых: (\d+)",
}
SET = re.compile(r"set\s+(\w+)\.(\w+)\s*=\s*(.+)$")
REFUSALS = re.compile(r"отказов \d+ \(([^)]*)\)")
IMPORT = re.compile(r"import\s+@(\w+)\s+v(\d+(?:\.\d+)*)")
UNDEF = "не хватает предпосылок"


def install_skills(home, text):
    """Скиллы задачи — пакеты-заглушки в доме ochan: версия из import,
    операции — строчные имена из provides:."""
    modules = os.path.join(home, ".octochan", "modules")
    shutil.rmtree(modules, ignore_errors=True)
    skills, current = {}, None
    for line in canonical(text).splitlines():
        if m := IMPORT.match(line):
            current = skills.setdefault(m.group(1), [m.group(2), []])
        elif line.startswith("  provides:") and current:
            current[1] += [n.strip() for n in line.split(":", 1)[1].split(",") if n.strip()[:1].islower()]
        elif line and not line.startswith(" "):
            current = None
    for name, (version, ops) in skills.items():
        os.makedirs(os.path.join(modules, name))
        parts = (version.split(".") + ["0", "0"])[:3]
        with open(os.path.join(modules, name, "package.yaml"), "w", encoding="utf-8") as f:
            f.write(f"name: {name}\nversion: {'.'.join(parts)}\n")
        with open(os.path.join(modules, name, f"{name}.ochan"), "w", encoding="utf-8") as f:
            f.write("".join(f"alias {op}() {{\n    return 0\n}}\n\n" for op in ops))


def sections(text):
    """Разделы верхнего уровня: [(заголовок, строки под ним)]."""
    out = []
    for line in text.splitlines():
        if line and not line[0].isspace():
            out.append((line.strip(), []))
        elif out and line.strip():
            out[-1][1].append(line.strip())
    return out


def decisions(task, answer):
    """Решения модели: set в ячейках, которых нет в задаче, и записи q: отчёта."""
    given_cells = {head for head, _ in sections(task)}
    made, told = [], []
    for head, body in sections(answer):
        if head.startswith("cell ") and head not in given_cells:
            made += [m.groups() for m in map(SET.match, body) if m]
        elif head == "report:":
            told += [line for line in body if line.startswith("q:")]
    return made, told


def in_language(check):
    """ochan fp check: отказов нет или среди них нет ⊥syn."""
    if check.returncode == 0:
        return True
    m = REFUSALS.search(check.stderr + check.stdout)
    return bool(m) and "⊥syn" not in m.group(1)


def consistent(task, answer):
    """Ответ сходится с задачей: решения новые и каждое — в report: с тем же значением."""
    given_cells = {head for head, _ in sections(task)}
    given = {m.group(1, 2) for _, body in sections(task) for m in map(SET.match, body) if m}
    made, told = [], []
    for head, body in sections(answer):
        if head.startswith("cell ") and head not in given_cells:
            made += [m.groups() for m in map(SET.match, body) if m]
        elif head == "report:":
            q = None
            for line in body:
                key, _, val = line.partition(":")
                if key == "q":
                    q = val.strip()
                elif key == "choice" and q is not None:
                    told.append((q, val.strip()))
    # Вопрос — параметр: одна запись покрывает все цели с тем же значением.
    return (not any((c, a) in given for c, a, _ in made) and len(told) == len(set(told))
            and {(a, v.strip()) for _, a, v in made} == set(told))


def ochan(args, env, stdin=None):
    return subprocess.run(args, input=stdin, capture_output=True, text=True, encoding="utf-8", env=env)


def summary(stderr):
    out = {"statements": 0, "tokens": 0, "calls": 0, "ring0": 0, "out_of_language": 0, "masked": 0,
           "rollbacks": 0, "dead": 0, "forced": 0, "chosen": 0}
    m = re.search(SUMMARY["statements"], stderr)
    if m:
        out["statements"], out["tokens"], out["calls"] = map(int, m.groups())
    m = re.search(SUMMARY["ring0"], stderr)
    if m:
        out["ring0"] = int(m.group(1))
    m = re.search(SUMMARY["language"], stderr)
    if m:
        out["out_of_language"], out["masked"] = map(int, m.groups())
    m = re.search(SUMMARY["rollbacks"], stderr)
    if m:
        out["rollbacks"], out["dead"] = map(int, m.groups())
    m = re.search(SUMMARY["logic"], stderr)
    if m:
        out["forced"], out["chosen"] = map(int, m.groups())
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--model", required=True)
    ap.add_argument("--ochan", required=True)
    ap.add_argument("--endpoint", default="http://127.0.0.1:8090")
    ap.add_argument("--home", required=True, help="дом ochan для замера: свой config.yaml и журнал")
    ap.add_argument("--tau", type=float, default=0.42)
    ap.add_argument("--max-new-tokens", type=int, default=1024)
    ap.add_argument("--out", required=True)
    ap.add_argument("tasks", nargs="+")
    args = ap.parse_args()

    os.makedirs(os.path.join(args.home, ".octochan"), exist_ok=True)
    config = os.path.join(args.home, "config.yaml")
    with open(config, "w", encoding="utf-8") as f:
        f.write(f"llm:\n  mode: embedded\n  engine: transformers\n  endpoint: {args.endpoint}\n"
                f"  rings:\n    tau: {args.tau}\n  max_tokens: 2048\n  retries: 32\n  timeout: 120s\n")
    env = dict(os.environ, HOME=args.home, USERPROFILE=args.home, OCTOCHAN_CONFIG=config)

    protocol = ochan([args.ochan, "fp", "protocol", "--mode", "formal"], env)
    if protocol.returncode != 0:
        raise SystemExit(protocol.stderr)
    engine = engine_for(args.model)

    tasks = [t for pattern in args.tasks for t in sorted(glob.glob(pattern))]
    rows = []
    with open(args.out, "w", encoding="utf-8") as out:
        for task in tasks:
            with open(task, encoding="utf-8") as f:
                text = f.read()
            prompt = protocol.stdout + TASK_MARKER + text
            install_skills(args.home, text)

            started = time.time()
            raw = engine.raw(prompt, args.max_new_tokens)
            raw_seconds = time.time() - started
            check = ochan([args.ochan, "fp", "check", "--model", "--name", "raw.fp", "-"], env, stdin=raw)

            started = time.time()
            gen = ochan([args.ochan, "llm", "generate", "--prompt", task], env)
            expected = (solve(text) or "").strip()
            ok = gen.returncode == 0
            row = {
                "task": os.path.basename(task),
                "missing": missing(text),
                "decisions_due": expected != copy_of(text).strip(),
                "raw_in_language": in_language(check),
                "raw_consistent": consistent(text, raw),
                "raw_exact": canonical(raw) == canonical(expected),
                "raw_decided": any(decisions(text, raw)),
                "raw_seconds": round(raw_seconds, 2),
                "raw": raw,
                "embedded_ok": ok,
                "embedded_consistent": ok and consistent(text, gen.stdout),
                "embedded_exact": ok and canonical(gen.stdout) == canonical(expected),
                "embedded_decided": ok and any(decisions(text, gen.stdout)),
                "embedded_seconds": round(time.time() - started, 2),
                "embedded_refused": UNDEF in gen.stderr,
                "embedded_reason": "" if gen.returncode == 0 else gen.stderr.strip()[-600:],
                "answer": gen.stdout,
                **summary(gen.stderr),
            }
            rows.append(row)
            out.write(json.dumps(row, ensure_ascii=False) + "\n")
            yes = lambda key: "да" if row[key] else "нет"
            print(f"{row['task']}: raw в ℒ — {yes('raw_in_language')}, сходится — {yes('raw_consistent')}, "
                  f"точно — {yes('raw_exact')}; embedded — {'ответ' if ok else 'нет ответа'}, "
                  f"сходится — {yes('embedded_consistent')}, точно — {yes('embedded_exact')}, "
                  f"утверждений {row['statements']}, "
                  f"откатов {row['rollbacks']}, вне ℒ {row['out_of_language']}, кольцо 0 {row['ring0']}, "
                  f"без токена {row['dead']}, логика {row['forced']}, выбор модели {row['chosen']}, "
                  f"{row['embedded_seconds']} с", flush=True)

    n = len(rows)
    total = lambda key: f"{sum(r[key] for r in rows)}/{n}"
    print(f"\nЗадач {n}: raw в ℒ {total('raw_in_language')}, сходится {total('raw_consistent')}, "
          f"точно {total('raw_exact')}; embedded с ответом {total('embedded_ok')}, "
          f"сходится {total('embedded_consistent')}, точно {total('embedded_exact')}; "
          f"утверждений логики {sum(r['forced'] for r in rows)}, выборов модели {sum(r['chosen'] for r in rows)}")
    lacking = [r for r in rows if r["missing"]]
    due = [r for r in rows if r["decisions_due"]]
    if lacking:
        # embedded без ответа на задаче без предпосылки — тоже не догадка.
        print(f"H3: без предпосылки {len(lacking)}, без догадки raw {sum(not r['raw_decided'] for r in lacking)}, "
              f"embedded {sum(not r['embedded_decided'] for r in lacking)} "
              f"(ochan отказал сам: {sum(r['embedded_refused'] for r in lacking)}); "
              f"решения нужны в {len(due)}, ложный отказ raw {sum(not r['raw_decided'] for r in due)}, "
              f"embedded {sum(r['embedded_ok'] and not r['embedded_decided'] for r in due)}")


if __name__ == "__main__":
    main()
