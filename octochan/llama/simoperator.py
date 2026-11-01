"""Имитатор оператора для замера: исход ответов архитектурной модели в
журнале решений ochan по corpus.solve.

На синтетике задуманный ответ известен — его выводит solve, — поэтому
исход можно отметить без человека: ответ верен, если его решения (строки
set новой ячейки) совпадают с решениями solve, порядок не важен. Исход
ставится тем же путём, что ставит оператор: ochan fp journal amend. Для
задач эксплуатации нужен человек; имитатор — только для проверки петли
«журнал → параметры модели» (fit.py).

    python simoperator.py --ochan ochan.exe --home G:/fpm-llm/runs/home \
        G:/fpm-llm/runs/own2-arch-gen-tau0.42.jsonl=G:/fpm-llm/runs/tasks-gen ...
"""

import argparse
import hashlib
import json
import os
import subprocess

from corpus import solve
from fpmtok import canonical


def decisions_of(text):
    return sorted(line for line in canonical(text).splitlines() if line.startswith("  set "))


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--ochan", required=True)
    ap.add_argument("--home", required=True, help="дом ochan замера")
    ap.add_argument("runs", nargs="+", help="JSONL measure.py=каталог его задач")
    args = ap.parse_args()

    # Ответ — по сумме текста: её ochan пишет в запись журнала (script_sum).
    expected = {}
    for run in args.runs:
        path, _, tasks = run.partition("=")
        for row in map(json.loads, open(path, encoding="utf-8")):
            if not row.get("embedded_ok"):
                continue
            task = open(os.path.join(tasks, row["task"]), encoding="utf-8").read()
            answer = row["answer"]
            expected[hashlib.sha256(answer.encode()).hexdigest()] = decisions_of(answer) == decisions_of(solve(task))

    env = dict(os.environ, HOME=args.home, USERPROFILE=args.home)
    journal = os.path.join(args.home, ".octochan", "decisions")
    marked = right = 0
    for name in sorted(os.listdir(journal)):
        with open(os.path.join(journal, name), encoding="utf-8") as f:
            record = json.load(f)
        if record.get("mode") != "architectural" or record.get("amendments") or record["script_sum"] not in expected:
            continue
        ok = expected[record["script_sum"]]
        verdict = "--right" if ok else "--wrong"
        reason = "имитатор оператора: решения " + ("совпадают" if ok else "расходятся") + " с solve"
        done = subprocess.run([args.ochan, "fp", "journal", "amend", record["id"], verdict, "--reason", reason],
                              capture_output=True, text=True, encoding="utf-8", env=env)
        if done.returncode != 0:
            raise SystemExit(done.stderr)
        marked += 1
        right += ok
    print(f"Отмечено исходов: {marked}, верных {right}")


if __name__ == "__main__":
    main()
