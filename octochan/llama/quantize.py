"""Квантование своей модели ℒ — шаг 4.

Веса матриц (вход, внимание, MLP; выход связан со входом) — симметрично по
группам строки: int8 (группа — вся строка) или int4 (группа --group, по
два значения в байте); масштаб группы — fp16. Нормировки остаются fp32.
Каталог — снова модель для adapter.py: config.json, fpmtok.json и
quant.safetensors с quant.json; при загрузке веса распаковываются.

Сверка с исходной моделью (--check) — на отложенных парах корпуса с
подстановкой верного префикса: доля позиций ответа, где самый вероятный
токен тот же, и потери на ответе. Полный замер — measure.py, как у любой
модели.

    python quantize.py --model G:/fpm-llm/models/own-small --bits 4 --out G:/fpm-llm/models/own-small-int4 \
        --check corpus-own.jsonl
"""

import argparse
import json
import os
import shutil

import torch
from safetensors.torch import load_file, save_file
from transformers import LlamaConfig, LlamaForCausalLM

import fpmtok

QUANT_JSON = "quant.json"
QUANT_WEIGHTS = "quant.safetensors"


def quantize(w, bits, group):
    """(q, scale): q — целые в [−2^(bits−1), 2^(bits−1) − 1], scale — на группу."""
    rows, cols = w.shape
    g = group if 0 < group < cols else cols
    x = w.float().view(rows, cols // g, g)
    top = 2 ** (bits - 1) - 1
    scale = (x.abs().amax(dim=-1, keepdim=True) / top).clamp(min=1e-12)
    q = torch.round(x / scale).clamp(-top - 1, top).to(torch.int8)
    return q.view(rows, cols), scale.squeeze(-1).half()


def dequantize(q, scale):
    rows, cols = q.shape
    g = cols // scale.shape[1]
    return (q.float().view(rows, -1, g) * scale.float().unsqueeze(-1)).view(rows, cols)


def pack4(q):
    u = (q.to(torch.int16) & 0xF).to(torch.uint8).view(-1)
    return (u[0::2] | (u[1::2] << 4)).contiguous()


def unpack4(packed, shape):
    u = torch.stack([packed & 0xF, packed >> 4], dim=-1).view(-1).to(torch.int8)
    return torch.where(u > 7, u - 16, u).view(shape)


def save(model_dir, out, bits, group):
    model = LlamaForCausalLM.from_pretrained(model_dir, dtype=torch.float32)
    tensors, quantized = {}, []
    for name, w in model.state_dict().items():
        if name == "lm_head.weight" and model.config.tie_word_embeddings:
            continue
        if w.dim() == 2:
            q, scale = quantize(w, bits, group)
            tensors[name + ".q"] = pack4(q) if bits == 4 else q
            tensors[name + ".scale"] = scale
            quantized.append([name, list(w.shape)])
        else:
            tensors[name] = w.float().contiguous()
    os.makedirs(out, exist_ok=True)
    save_file(tensors, os.path.join(out, QUANT_WEIGHTS))
    with open(os.path.join(out, QUANT_JSON), "w", encoding="utf-8") as f:
        json.dump({"bits": bits, "group": group, "tensors": quantized}, f)
    for name in ("config.json", fpmtok.FILE):
        shutil.copy(os.path.join(model_dir, name), os.path.join(out, name))


def is_quantized(path):
    return os.path.isfile(os.path.join(path, QUANT_JSON))


def load(path, dtype=torch.float32):
    """Модель из квантованного каталога: веса распакованы в dtype."""
    with open(os.path.join(path, QUANT_JSON), encoding="utf-8") as f:
        meta = json.load(f)
    raw = load_file(os.path.join(path, QUANT_WEIGHTS))
    state = {k: v for k, v in raw.items() if not k.endswith((".q", ".scale"))}
    for name, shape in meta["tensors"]:
        q = raw[name + ".q"]
        q = unpack4(q, shape) if meta["bits"] == 4 else q
        state[name] = dequantize(q, raw[name + ".scale"])
    model = LlamaForCausalLM(LlamaConfig.from_pretrained(path))
    missing, unexpected = model.load_state_dict(state, strict=False)
    if unexpected or any(k != "lm_head.weight" for k in missing):
        raise ValueError(f"веса не сошлись с моделью: нет {missing}, лишние {unexpected}")
    model.tie_weights()
    return model.to(dtype)


def size(path, names):
    return sum(os.path.getsize(os.path.join(path, n)) for n in names if os.path.isfile(os.path.join(path, n)))


@torch.no_grad()
def compare(base, other, tok, corpus, pairs, device):
    """Доля позиций ответа с тем же argmax и потери на ответе у обеих моделей."""
    rows = []
    with open(corpus, encoding="utf-8") as f:
        for row in map(json.loads, f):
            if row.get("split") == "test":
                rows.append(row)
            if len(rows) == pairs:
                break
    same = total = 0
    loss = [0.0, 0.0]
    for row in rows:
        ids, start, _ = tok.pair(row["task"], row["answer"])
        x = torch.tensor([ids[:-1]], device=device)
        target = torch.tensor(ids[start:], device=device)
        outs = [m(input_ids=x).logits[0, start - 1:].float() for m in (base, other)]
        same += int((outs[0].argmax(-1) == outs[1].argmax(-1)).sum())
        total += len(target)
        for k, logits in enumerate(outs):
            loss[k] += float(torch.nn.functional.cross_entropy(logits, target, reduction="sum"))
    return same / total, loss[0] / total, loss[1] / total, len(rows)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--model", required=True, help="своя модель ℒ (train_own.py)")
    ap.add_argument("--bits", type=int, choices=[4, 8], default=8)
    ap.add_argument("--group", type=int, default=0, help="размер группы; 0 — вся строка (int4: 32 по умолчанию)")
    ap.add_argument("--out", required=True)
    ap.add_argument("--check", help="корпус: сверка с исходной моделью на отложенных парах")
    ap.add_argument("--pairs", type=int, default=500)
    args = ap.parse_args()
    group = args.group or (32 if args.bits == 4 else 0)

    save(args.model, args.out, args.bits, group)
    before = size(args.model, ["model.safetensors"])
    after = size(args.out, [QUANT_WEIGHTS])
    print(f"int{args.bits}, группа {group or 'строка'}: веса {before / 2**20:.1f} → {after / 2**20:.1f} МБ "
          f"({before / after:.1f}×)", flush=True)
    if args.check:
        device = "cuda" if torch.cuda.is_available() else "cpu"
        base = LlamaForCausalLM.from_pretrained(args.model, dtype=torch.float32).to(device).eval()
        other = load(args.out).to(device).eval()
        agree, loss_base, loss_q, n = compare(base, other, fpmtok.Tokenizer.load(args.model), args.check, args.pairs, device)
        print(f"Сверка на {n} отложенных парах: тот же токен в {agree:.2%} позиций ответа; "
              f"потери на ответе {loss_base:.4f} → {loss_q:.4f}", flush=True)


if __name__ == "__main__":
    main()
