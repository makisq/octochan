"""Обучение своей модели ℒ с нуля: архитектура Llama, словарь — fpmtok.

Обучение двойное. Статистическое — только по конечному словарю языка:
модель учит последовательности токенов, из которых складываются задачи —
определения обобщений, отношения, правила, — и ответы, поэтому потери
считаются по всей паре, а не только по ответу. Имена частных случаев в нём
— переменные: их модель не учит, а переносит. Логическое — сами ответы: их
выводит corpus.solve (перенос по отношениям, приоритеты, наследование по
стрелке, отказ без предпосылки), и каждую пару принимает разбор ochan.

Пара — <bos> задача <answer> ответ <eos>, без протокола. Батч собирается по
числу токенов (--batch-tokens), пары близкой длины — вместе.

    ochan fp alphabet > alphabet.tsv
    python train_own.py --alphabet alphabet.tsv --corpus corpus-own.jsonl --out G:/fpm-llm/models/own-small

Каждые --save-every шагов out — рабочая модель и состояние обучения;
--resume продолжает с последнего сохранения.

--init — дообучение: веса и словарь — готовой модели (например, на парах
кластера инцидентов с примесью базового корпуса, learn.py --pairs). Новые
веса пишутся в --out, прежний каталог остаётся — откат к нему; параметры
колец (rings.json и его версии) переходят с весами.

    python train_own.py --init G:/fpm-llm/models/own-small-2 --corpus pairs.jsonl \\
        --out G:/fpm-llm/models/own-small-3 --epochs 1 --lr 1e-4
"""

import argparse
import contextlib
import json
import math
import os
import random
import shutil
import time

import torch
from transformers import LlamaForCausalLM

import fpmtok
import own
import quantize
import rings

STATE = "train_state.pt"


def examples(tok, corpus, split, context):
    out, skipped = [], 0
    with open(corpus, encoding="utf-8") as f:
        for row in map(json.loads, f):
            if row.get("split", "train") != split:
                continue
            ids, start, _ = tok.pair(row["task"], row["answer"])
            if len(ids) > context:
                skipped += 1
                continue
            out.append((ids, start))
    return out, skipped


def batches(data, budget, rng):
    """Пары близкой длины вместе; в батче не больше budget токенов с выравниванием."""
    order = sorted(range(len(data)), key=lambda k: (len(data[k][0]), rng.random()))
    out, batch, longest = [], [], 0
    for k in order:
        n = len(data[k][0])
        if batch and max(longest, n) * (len(batch) + 1) > budget:
            out.append(batch)
            batch, longest = [], 0
        batch.append(k)
        longest = max(longest, n)
    if batch:
        out.append(batch)
    rng.shuffle(out)
    return out


def tensors(data, batch, pad, device):
    """Вход, цели (−100 вне текста) и маска ответа."""
    width = max(len(data[k][0]) for k in batch)
    ids = torch.full((len(batch), width), pad, dtype=torch.long)
    labels = torch.full((len(batch), width), -100, dtype=torch.long)
    answer = torch.zeros((len(batch), width), dtype=torch.bool)
    for row, k in enumerate(batch):
        seq, start = data[k]
        ids[row, :len(seq)] = torch.tensor(seq)
        labels[row, :len(seq)] = torch.tensor(seq)
        answer[row, start:len(seq)] = True
    return ids.to(device), labels.to(device), answer.to(device)


def autocast(device):
    """bf16 — только на видеокарте; на процессоре считается в fp32."""
    if str(device).startswith("cuda"):
        return torch.autocast("cuda", dtype=torch.bfloat16)
    return contextlib.nullcontext()


def losses(model, ids, labels, answer):
    """Средние потери по всей паре и по ответу."""
    with autocast(ids.device):
        logits = model(input_ids=ids, attention_mask=(labels != -100).long()).logits
    logits, labels, answer = logits[:, :-1].float(), labels[:, 1:], answer[:, 1:]
    each = torch.nn.functional.cross_entropy(logits.reshape(-1, logits.shape[-1]), labels.reshape(-1),
                                             ignore_index=-100, reduction="none").view(labels.shape)
    text = labels != -100
    return each[text].mean(), each[answer & text].mean()


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--alphabet", help="ochan fp alphabet: словарь новой модели")
    ap.add_argument("--init", help="дообучение: начать с весов и словаря этой модели")
    ap.add_argument("--corpus", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--size", default="small", choices=sorted(own.SIZES))
    ap.add_argument("--context", type=int, default=1024)
    ap.add_argument("--epochs", type=float, default=8)
    ap.add_argument("--batch-tokens", type=int, default=32768)
    ap.add_argument("--lr", type=float, default=1e-3)
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--save-every", type=int, default=500, help="шагов между сохранениями")
    ap.add_argument("--resume", action="store_true", help="продолжить с сохранения в --out")
    ap.add_argument("--device", default="cuda" if torch.cuda.is_available() else "cpu",
                    help="cuda или cpu; по умолчанию — видеокарта, если она есть")
    args = ap.parse_args()

    torch.manual_seed(args.seed)
    rng = random.Random(args.seed)
    device = args.device
    cuda = device.startswith("cuda")
    if cuda and not torch.cuda.is_available():
        raise SystemExit(f"--device {device}: видеокарты с CUDA нет — --device cpu")
    state_path = os.path.join(args.out, STATE)
    resume = args.resume and os.path.isfile(state_path)
    if args.init and os.path.abspath(args.init) == os.path.abspath(args.out):
        raise SystemExit("--out должен быть новым каталогом: прежние веса остаются для отката")
    if args.init and quantize.is_quantized(args.init):
        raise SystemExit("дообучение — с полных весов, не квантованных")
    if resume:
        tok = fpmtok.Tokenizer.load(args.out)
    elif args.init:
        tok = fpmtok.Tokenizer.load(args.init)
        os.makedirs(args.out, exist_ok=True)
        if os.path.isfile(os.path.join(args.init, rings.FILE)):
            shutil.copyfile(os.path.join(args.init, rings.FILE), os.path.join(args.out, rings.FILE))
        if os.path.isdir(os.path.join(args.init, rings.VERSIONS)):
            shutil.copytree(os.path.join(args.init, rings.VERSIONS), os.path.join(args.out, rings.VERSIONS),
                            dirs_exist_ok=True)
    elif args.alphabet:
        with open(args.alphabet, encoding="utf-8") as f:
            tok = fpmtok.Tokenizer.build(f.read())
    else:
        raise SystemExit("нужно --alphabet (новая модель) или --init (дообучение)")

    train, skipped = examples(tok, args.corpus, "train", args.context)
    test, _ = examples(tok, args.corpus, "test", args.context)
    print(f"Пар: обучение {len(train)}, отложено {len(test)}, длиннее контекста {skipped}; "
          f"токенов в паре ~{sum(len(e[0]) for e in train) // max(1, len(train))}, словарь {len(tok)}", flush=True)

    model = (LlamaForCausalLM.from_pretrained(args.out) if resume
             else LlamaForCausalLM.from_pretrained(args.init) if args.init
             else LlamaForCausalLM(own.config(tok, args.size, args.context))).to(device)
    model.train()
    print(f"Модель {args.size}: {sum(p.numel() for p in model.parameters()) / 1e6:.1f} млн параметров", flush=True)

    per_epoch = len(batches(train, args.batch_tokens, random.Random(0)))
    steps = math.ceil(per_epoch * args.epochs)
    decay = [p for n, p in model.named_parameters() if p.dim() >= 2]
    rest = [p for n, p in model.named_parameters() if p.dim() < 2]
    opt = torch.optim.AdamW([{"params": decay, "weight_decay": 0.1}, {"params": rest, "weight_decay": 0.0}],
                            lr=args.lr, betas=(0.9, 0.95))
    warmup = max(1, min(500, steps // 20))
    sched = torch.optim.lr_scheduler.LambdaLR(
        opt, lambda s: min(1.0, (s + 1) / warmup) * (0.1 + 0.9 * 0.5 * (1 + math.cos(math.pi * min(1.0, s / steps)))))

    def evaluate():
        model.eval()
        total, ans, n = 0.0, 0.0, 0
        with torch.no_grad():
            for batch in batches(test, args.batch_tokens, random.Random(0)):
                a, b = losses(model, *tensors(test, batch, tok.pad, device))
                total, ans, n = total + float(a) * len(batch), ans + float(b) * len(batch), n + len(batch)
        model.train()
        return total / max(1, n), ans / max(1, n)

    def save(step, queue):
        model.save_pretrained(args.out)
        tok.save(args.out)
        torch.save({"step": step, "queue": queue, "rng": rng.getstate(), "opt": opt.state_dict(),
                    "sched": sched.state_dict(), "torch": torch.get_rng_state(),
                    "cuda": torch.cuda.get_rng_state() if cuda else None}, state_path)

    first, queue = 0, []
    if resume:
        state = torch.load(state_path, weights_only=False)
        opt.load_state_dict(state["opt"])
        sched.load_state_dict(state["sched"])
        rng.setstate(state["rng"])
        torch.set_rng_state(state["torch"])
        if cuda and state.get("cuda") is not None:
            torch.cuda.set_rng_state(state["cuda"])
        first, queue = state["step"], state["queue"]
        print(f"Продолжение с шага {first}/{steps}", flush=True)
    else:
        print("Потери на отложенных до обучения: пара {:.3f}, ответ {:.3f}".format(*evaluate()), flush=True)

    started, running = time.time(), []
    for step in range(first, steps):
        if not queue:
            queue = batches(train, args.batch_tokens, rng)
        loss, _ = losses(model, *tensors(train, queue.pop(), tok.pad, device))
        loss.backward()
        torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
        opt.step()
        sched.step()
        opt.zero_grad(set_to_none=True)
        running.append(float(loss.detach()))
        if (step + 1) % 100 == 0 or step + 1 == steps:
            print(f"шаг {step + 1}/{steps} (эпоха {(step + 1) / per_epoch:.1f}): потери {sum(running) / len(running):.3f}, "
                  f"{time.time() - started:.0f} с", flush=True)
            running = []
        if (step + 1) % args.save_every == 0 and step + 1 < steps:
            save(step + 1, queue)
            print("  отложенные: пара {:.3f}, ответ {:.3f}".format(*evaluate()), flush=True)
    print("Потери на отложенных после обучения: пара {:.3f}, ответ {:.3f}".format(*evaluate()), flush=True)
    save(steps, queue)
    print(f"Сохранено: {args.out}", flush=True)


if __name__ == "__main__":
    main()
