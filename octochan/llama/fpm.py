"""Модель в ФПм-виде: базовая модель с выходным словарём, обрезанным до ℒ.

Каталог модели — не копия весов, а правка поверх базовой:

    fpm.json            {"base": "meta-llama/Llama-3.2-1B-Instruct",
                         "alphabet": [id, ...], "end": [id, ...], "lora": null}
    lm_head.safetensors выходной слой: строки исходного слоя для токенов алфавита

alphabet — идентификаторы базового токенизатора, которые модель может
написать; номер в alphabet — номер выхода нового lm_head. Входной словарь
остаётся целым: модель читает задачу оператора и протокол как есть, а
написать может только токены алфавита. Эмбеддинги у Llama 3.2 связаны с
выходом, поэтому связь рвётся: вход — прежняя матрица, выход — её строки.
"""

import json
import os

import torch
from safetensors.torch import load_file, save_file
from transformers import AutoModelForCausalLM, AutoTokenizer

FPM_JSON = "fpm.json"
LM_HEAD = "lm_head.safetensors"


def is_fpm(path):
    return os.path.isfile(os.path.join(path, FPM_JSON))


def load(path, device="cuda", dtype=torch.bfloat16):
    """Модель и токенизатор: базовые или в ФПм-виде.

    Возвращает (model, tokenizer, alphabet), где alphabet — тензор исходных
    идентификаторов по номерам выходов или None, если словарь не обрезан.
    """
    if not is_fpm(path):
        tok = AutoTokenizer.from_pretrained(path)
        model = AutoModelForCausalLM.from_pretrained(path, dtype=dtype).to(device).eval()
        return model, tok, None
    with open(os.path.join(path, FPM_JSON), encoding="utf-8") as f:
        meta = json.load(f)
    tok = AutoTokenizer.from_pretrained(meta["base"])
    model = AutoModelForCausalLM.from_pretrained(meta["base"], dtype=dtype)
    # Выходной слой — свой, обученный вместе с LoRA; LoRA — на внимании и
    # MLP, поверх уже обрезанного выхода.
    set_head(model, load_file(os.path.join(path, LM_HEAD))["weight"].to(dtype))
    if meta.get("lora"):
        from peft import PeftModel

        model = PeftModel.from_pretrained(model, os.path.join(path, meta["lora"])).merge_and_unload()
    model = model.to(device).eval()
    return model, tok, torch.tensor(meta["alphabet"], device=device)


def set_head(model, weight):
    """Заменяет выходной слой на weight [len(alphabet), hidden], разрывая связь с входом."""
    hidden = weight.shape[1]
    head = torch.nn.Linear(hidden, weight.shape[0], bias=False, dtype=weight.dtype)
    head.weight = torch.nn.Parameter(weight.clone())
    model.config.tie_word_embeddings = False
    if hasattr(model, "_tied_weights_keys"):
        model._tied_weights_keys = []
    model.set_output_embeddings(head)


def save(path, base, alphabet, end, head, lora=None):
    os.makedirs(path, exist_ok=True)
    with open(os.path.join(path, FPM_JSON), "w", encoding="utf-8") as f:
        json.dump({"base": base, "alphabet": list(alphabet), "end": list(end), "lora": lora}, f)
    save_file({"weight": head.contiguous().cpu()}, os.path.join(path, LM_HEAD))
