"""Дообучение своей модели на корпусе ℒ (LoRA) — шаг 3.

Вход — ровно то, что модель получает от ochan во встроенном режиме: протокол
(system) и задача (user). Цель — ответ на ℒ и конец реплики; потери только
по ответу. Учатся LoRA на внимании и MLP и выходной слой, уже обрезанный до
ℒ (prune.py): у модели нет выхода вне алфавита, и учиться выбирать среди
чужих токенов ей незачем.

Результат — снова модель в ФПм-виде: fpm.json, обученный lm_head.safetensors
и каталог lora поверх той же базовой модели.

    ochan fp protocol --mode formal > protocol.txt
    python train.py --model G:/fpm-llm/models/smollm2-135m-fpm --protocol protocol.txt \
        --corpus corpus.jsonl --out G:/fpm-llm/models/smollm2-135m-fpm-lora

Каждые --save-every шагов out — рабочая модель и состояние обучения
(train_state.pt). Драйвер видеокарты под Windows сбрасывает её (TDR), и
процесс теряет CUDA; --resume продолжает с последнего сохранения.
"""

import argparse
import json
import math
import os
import random
import time

import torch
from peft import LoraConfig, get_peft_model, set_peft_model_state_dict
from safetensors.torch import load_file

import fpm
from adapter import TASK_MARKER, answer_ids, prompt_ids, turn_end

TARGETS = ["q_proj", "k_proj", "v_proj", "o_proj", "gate_proj", "up_proj", "down_proj"]
STATE = "train_state.pt"


def examples(tok, protocol, corpus, split, index, end):
    out, skipped = [], 0
    with open(corpus, encoding="utf-8") as f:
        for row in map(json.loads, f):
            if row.get("split", "train") != split:
                continue
            prompt = prompt_ids(tok, protocol + TASK_MARKER + row["task"])
            answer = answer_ids(tok, row["answer"]) + [end]
            if any(t not in index for t in answer):
                skipped += 1  # токена ответа нет в алфавите: пересоберите prune.py с корпусом
                continue
            out.append((prompt + answer, len(prompt), [index[t] for t in answer]))
    return out, skipped


def loss_on(model, ids, start, targets, device):
    """Средняя перекрёстная энтропия по токенам ответа."""
    logits = model(input_ids=torch.tensor([ids[:-1]], device=device)).logits[0, start - 1:]
    return torch.nn.functional.cross_entropy(logits.float(), torch.tensor(targets, device=device))


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--model", required=True, help="модель в ФПм-виде (prune.py)")
    ap.add_argument("--protocol", required=True, help="ochan fp protocol --mode formal")
    ap.add_argument("--corpus", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--epochs", type=float, default=1.0)
    ap.add_argument("--lr", type=float, default=2e-4)
    ap.add_argument("--accum", type=int, default=8)
    ap.add_argument("--rank", type=int, default=16)
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--checkpointing", action="store_true", help="экономия памяти на большой модели")
    ap.add_argument("--save-every", type=int, default=50, help="шагов между сохранениями")
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
    with open(os.path.join(args.model, fpm.FPM_JSON), encoding="utf-8") as f:
        meta = json.load(f)
    with open(args.protocol, encoding="utf-8") as f:
        protocol = f.read()
    model, tok, alphabet = fpm.load(args.model, device)
    alphabet = alphabet.tolist()
    index = {t: k for k, t in enumerate(alphabet)}
    end = turn_end(model, tok)

    train, skipped = examples(tok, protocol, args.corpus, "train", index, end)
    test, _ = examples(tok, protocol, args.corpus, "test", index, end)
    print(f"Пар: обучение {len(train)}, отложено {len(test)}, пропущено {skipped}; "
          f"токенов в паре ~{sum(len(e[0]) for e in train) // max(1, len(train))}", flush=True)

    model.train()
    if args.checkpointing:
        model.gradient_checkpointing_enable()
        model.enable_input_require_grads()
    model = get_peft_model(model, LoraConfig(r=args.rank, lora_alpha=2 * args.rank, lora_dropout=0.05,
                                             target_modules=TARGETS, task_type="CAUSAL_LM"))
    head = model.get_output_embeddings()
    head.weight.requires_grad_(True)
    params = [p for p in model.parameters() if p.requires_grad]
    print(f"Учатся: {sum(p.numel() for p in params) / 1e6:.2f} млн параметров "
          f"(LoRA r={args.rank} и выходной слой {head.weight.shape[0]}×{head.weight.shape[1]})", flush=True)

    steps = math.ceil(len(train) * args.epochs / args.accum)
    opt = torch.optim.AdamW(params, lr=args.lr, weight_decay=0.0)
    warmup = max(1, steps // 20)
    sched = torch.optim.lr_scheduler.LambdaLR(
        opt, lambda s: min(1.0, (s + 1) / warmup) * 0.5 * (1 + math.cos(math.pi * min(1.0, s / steps))))

    def evaluate():
        model.eval()
        with torch.no_grad():
            value = sum(float(loss_on(model, *e, device)) for e in test) / max(1, len(test))
        model.train()
        return value

    def save(step, order):
        model.save_pretrained(os.path.join(args.out, "lora"))
        fpm.save(args.out, meta["base"], alphabet, meta["end"], head.weight.detach().to(torch.bfloat16), lora="lora")
        torch.save({"step": step, "order": order, "rng": rng.getstate(), "opt": opt.state_dict(),
                    "sched": sched.state_dict(), "torch": torch.get_rng_state(),
                    "cuda": torch.cuda.get_rng_state() if cuda else None}, os.path.join(args.out, STATE))

    first, order = 0, []
    state_path = os.path.join(args.out, STATE)
    if args.resume and os.path.isfile(state_path):
        state = torch.load(state_path, weights_only=False)
        set_peft_model_state_dict(model, load_file(os.path.join(args.out, "lora", "adapter_model.safetensors")))
        head.weight.data.copy_(load_file(os.path.join(args.out, fpm.LM_HEAD))["weight"])
        opt.load_state_dict(state["opt"])
        sched.load_state_dict(state["sched"])
        rng.setstate(state["rng"])
        torch.set_rng_state(state["torch"])
        if cuda and state.get("cuda") is not None:
            torch.cuda.set_rng_state(state["cuda"])
        first, order = state["step"], state["order"]
        print(f"Продолжение с шага {first}/{steps}", flush=True)
    else:
        print(f"Потери на отложенных до обучения: {evaluate():.3f}", flush=True)

    started, running = time.time(), []
    for step in range(first, steps):
        for _ in range(args.accum):
            if not order:
                order = list(range(len(train)))
                rng.shuffle(order)
            loss = loss_on(model, *train[order.pop()], device)
            (loss / args.accum).backward()
            running.append(float(loss.detach()))
        torch.nn.utils.clip_grad_norm_(params, 1.0)
        opt.step()
        sched.step()
        opt.zero_grad(set_to_none=True)
        if (step + 1) % 25 == 0 or step + 1 == steps:
            print(f"шаг {step + 1}/{steps}: потери {sum(running) / len(running):.3f}, "
                  f"{time.time() - started:.0f} с", flush=True)
            running = []
        if (step + 1) % args.save_every == 0 and step + 1 < steps:
            save(step + 1, order)
    print(f"Потери на отложенных после обучения: {evaluate():.3f}", flush=True)

    save(steps, order)
    print(f"Сохранено: {args.out}", flush=True)


if __name__ == "__main__":
    main()
