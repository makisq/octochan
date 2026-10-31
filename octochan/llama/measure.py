"""Замер шага: сырой вывод модели против встроенного режима ochan.

По каждой задаче (формальному промпту оператора):

- raw — модель пишет ответ сама, жадно, без колец и маски; ответ
  проверяется так же, как декодер принимает готовый текст:
  ochan fp check --model;
- embedded — ответ ведёт ochan (llm generate) через адаптер по протоколу
  v1: кольцо 0, маска языка, откаты. Из сводки колец берутся откаты,
  утверждения вне ℒ, отклонённые кольцом 0, позиции без токена.

ochan запускается в отдельном доме (--home): журнал и роли пользователя
замер не трогает. Адаптер должен быть запущен (adapter.py serve).

    python measure.py --model HuggingFaceTB/SmolLM2-135M-Instruct --ochan ochan.exe \
        --endpoint http://127.0.0.1:8090 --home G:/fpm-llm/runs/home --out run.jsonl tasks/*.fp
"""

import argparse
import glob
import json
import os
import re
import subprocess
import time

from adapter import TASK_MARKER, Engine

SUMMARY = {
    "statements": r"Утверждений: (\d+), токенов: (\d+), обращений к модели: (\d+)",
    "ring0": r"Кольцо 0:\s+отклонено (\d+)",
    "language": r"утверждений вне ℒ (\d+), токенов вне языка (\d+)",
    "rollbacks": r"Откатов:\s+(\d+) к началу утверждения; позиций без токена выше порога: (\d+)",
}


def ochan(args, env, stdin=None):
    return subprocess.run(args, input=stdin, capture_output=True, text=True, encoding="utf-8", env=env)


def summary(stderr):
    out = {"statements": 0, "tokens": 0, "calls": 0, "ring0": 0, "out_of_language": 0, "masked": 0,
           "rollbacks": 0, "dead": 0}
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
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--model", required=True)
    ap.add_argument("--ochan", required=True)
    ap.add_argument("--endpoint", default="http://127.0.0.1:8090")
    ap.add_argument("--home", required=True, help="дом ochan для замера: свой config.yaml и журнал")
    ap.add_argument("--tau", type=float, default=0.42)
    ap.add_argument("--max-new-tokens", type=int, default=384)
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
    engine = Engine(args.model)

    tasks = [t for pattern in args.tasks for t in sorted(glob.glob(pattern))]
    rows = []
    with open(args.out, "w", encoding="utf-8") as out:
        for task in tasks:
            with open(task, encoding="utf-8") as f:
                prompt = protocol.stdout + TASK_MARKER + f.read()

            started = time.time()
            raw = engine.raw(prompt, args.max_new_tokens)
            raw_seconds = time.time() - started
            check = ochan([args.ochan, "fp", "check", "--model", "--name", "raw.fp", "-"], env, stdin=raw)

            started = time.time()
            gen = ochan([args.ochan, "llm", "generate", "--prompt", task], env)
            row = {
                "task": os.path.basename(task),
                "raw_in_language": check.returncode == 0,
                "raw_seconds": round(raw_seconds, 2),
                "raw_head": raw[:160],
                "embedded_ok": gen.returncode == 0,
                "embedded_seconds": round(time.time() - started, 2),
                "embedded_reason": "" if gen.returncode == 0 else gen.stderr.strip().splitlines()[-1],
                "answer": gen.stdout,
                **summary(gen.stderr),
            }
            rows.append(row)
            out.write(json.dumps(row, ensure_ascii=False) + "\n")
            print(f"{row['task']}: raw в ℒ — {'да' if row['raw_in_language'] else 'нет'}; embedded — "
                  f"{'ответ' if row['embedded_ok'] else 'нет ответа'}, утверждений {row['statements']}, "
                  f"откатов {row['rollbacks']}, вне ℒ {row['out_of_language']}, кольцо 0 {row['ring0']}, "
                  f"без токена {row['dead']}, {row['embedded_seconds']} с", flush=True)

    n = len(rows)
    print(f"\nЗадач {n}: raw в ℒ {sum(r['raw_in_language'] for r in rows)}/{n}, "
          f"embedded с ответом {sum(r['embedded_ok'] for r in rows)}/{n}")


if __name__ == "__main__":
    main()
