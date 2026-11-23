"""Эталон обучения для go test ./own: потери, градиенты и три шага AdamW от torch.

Микромодель Llama (головы внимания 4, головы KV 2 — с группами), три пары
разной длины. Потери и батч считает сам llama/train_own.py — тест сверяет
обучение на Go (own/train.go) с ним, а не с пересказом.

    python own/testdata/train/make.py

Пишет рядом: config.json, model.safetensors (веса до обучения),
grads.safetensors (градиент среднего по батчу), after.safetensors (веса
после трёх шагов) и expected.json.
"""
import json
import math
import os
import sys

import torch
from safetensors.torch import save_file
from transformers import LlamaConfig, LlamaForCausalLM

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "..", "..", "llama"))
import train_own  # noqa: E402

LR, STEPS = 0.01, 3

torch.manual_seed(11)
config = LlamaConfig(vocab_size=48, hidden_size=32, num_hidden_layers=2, num_attention_heads=4, num_key_value_heads=2,
                     head_dim=8, intermediate_size=48, max_position_embeddings=64, rms_norm_eps=1e-6,
                     tie_word_embeddings=True, bos_token_id=1, eos_token_id=3, pad_token_id=0, hidden_act="silu")
model = LlamaForCausalLM(config)
with torch.no_grad():
    for name, p in model.named_parameters():
        if p.dim() >= 2:
            p.copy_(torch.randn_like(p) * 0.25)   # внимание и MLP — не около нуля
        else:
            p.copy_(1 + 0.2 * torch.randn_like(p))  # нормировки — не единицы
model.save_pretrained(HERE)
for extra in ("generation_config.json",):
    path = os.path.join(HERE, extra)
    if os.path.exists(path):
        os.remove(path)

gen = torch.Generator().manual_seed(5)
data = []
for n, start in ((9, 5), (14, 8), (6, 3)):
    ids = [1] + torch.randint(4, 48, (n - 2,), generator=gen).tolist() + [3]
    data.append((ids, start))
batch = [0, 1, 2]


def fresh():
    m = LlamaForCausalLM.from_pretrained(HERE)
    m.train()
    return m


model = fresh()
loss, answer = train_own.losses(model, *train_own.tensors(data, batch, 0, "cpu"))
loss.backward()
grads = {n: p.grad.clone() for n, p in model.named_parameters()}
norm = float(torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0))
save_file(grads, os.path.join(HERE, "grads.safetensors"))

# Три шага, как в цикле train_own.py: AdamW, обрезка нормы, расписание.
model = fresh()
decay = [p for n, p in model.named_parameters() if p.dim() >= 2]
rest = [p for n, p in model.named_parameters() if p.dim() < 2]
opt = torch.optim.AdamW([{"params": decay, "weight_decay": 0.1}, {"params": rest, "weight_decay": 0.0}],
                        lr=LR, betas=(0.9, 0.95))
warmup = max(1, min(500, STEPS // 20))
sched = torch.optim.lr_scheduler.LambdaLR(
    opt, lambda s: min(1.0, (s + 1) / warmup) * (0.1 + 0.9 * 0.5 * (1 + math.cos(math.pi * min(1.0, s / STEPS)))))
trace = []
for step in range(STEPS):
    step_loss, _ = train_own.losses(model, *train_own.tensors(data, batch, 0, "cpu"))
    step_loss.backward()
    torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
    opt.step()
    sched.step()
    opt.zero_grad(set_to_none=True)
    trace.append(float(step_loss.detach()))
save_file({n: p.detach().clone() for n, p in model.named_parameters()}, os.path.join(HERE, "after.safetensors"))

with open(os.path.join(HERE, "expected.json"), "w", encoding="utf-8", newline="\n") as f:
    json.dump({"pairs": [{"ids": ids, "start": start} for ids, start in data], "loss": float(loss), "answer": float(answer),
               "norm": norm, "lr": LR, "steps": STEPS, "losses": trace,
               "torch": torch.__version__}, f, indent=1)
    f.write("\n")
print(f"потери {float(loss):.6f}, ответ {float(answer):.6f}, норма градиента {norm:.4f}, по шагам {trace}")
