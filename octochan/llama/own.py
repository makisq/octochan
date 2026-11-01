"""Своя модель ℒ: архитектура Llama, веса с нуля, словарь — fpmtok.

Модель не дообучается из чужой: её веса и словарь — только ℒ. Протокол она
не читает — язык у неё в весах; из prompt ochan берётся только задача (после
TASK_MARKER) в канонической записи, имена частных случаев — переменные.

Engine — тот же протокол движка v1, что у adapter.Engine, и тот же
детерминированный счёт распределения. Кандидаты — текстом: переменная —
имя частного случая из задачи, следующая свободная — новое имя cK.
Переменная дальше следующей свободной и одиночный байт запасного хода
текста не имеют, их в распределении нет.

    python adapter.py serve --model G:/fpm-llm/models/own-small --listen 127.0.0.1:8093
"""

import math
import os

import torch
from transformers import LlamaConfig, LlamaForCausalLM

import fpmtok
from adapter import TASK_MARKER
from adapter import Engine as Base

# Ширина, слои, головы внимания, ширина MLP.
SIZES = {
    "tiny": (256, 6, 4, 704),
    "small": (384, 8, 6, 1024),
    "base": (512, 12, 8, 1408),
}


def config(tok, size, context):
    hidden, layers, heads, inter = SIZES[size]
    return LlamaConfig(vocab_size=len(tok), hidden_size=hidden, num_hidden_layers=layers,
                       num_attention_heads=heads, num_key_value_heads=heads, intermediate_size=inter,
                       max_position_embeddings=context, tie_word_embeddings=True,
                       pad_token_id=tok.pad, bos_token_id=tok.bos, eos_token_id=tok.eos)


def is_own(path):
    return os.path.isfile(os.path.join(path, fpmtok.FILE))


def load_model(path, dtype=torch.float32):
    """Веса своей модели: обычные или квантованные (quantize.py)."""
    import quantize

    if quantize.is_quantized(path):
        return quantize.load(path, dtype)
    return LlamaForCausalLM.from_pretrained(path, dtype=dtype)


def task_of(prompt):
    """Задача из prompt ochan: протокол своей модели не нужен."""
    _, sep, task = prompt.partition(TASK_MARKER)
    return task if sep else prompt


class Engine(Base):
    def __init__(self, model, device="cuda", dtype="float32"):
        self.name = model
        self.device = device
        self.tok = fpmtok.Tokenizer.load(model)
        self.model = load_model(model, getattr(torch, dtype)).to(device).eval()
        self.chat = False
        self.to_id = None
        self.end_out = [self.tok.eos]
        self.special = torch.tensor([self.tok.pad, self.tok.bos, self.tok.answer], device=device)
        number = [-1] * len(self.tok)
        for token, (_, k) in self.tok.variable.items():
            number[token] = k
        self.number = torch.tensor(number, device=device)
        self.bytes = torch.tensor(sorted(self.tok.byte), device=device)
        self._prompt = (None, None)
        self.names = fpmtok.Names()
        self.cache, self.cache_ids = None, []
        self.paths, self.proposed = {"": []}, {}

    def prompt_ids(self, prompt):
        if self._prompt[0] != prompt:
            ids, _, self.names = self.tok.pair(task_of(prompt))
            self._prompt = (prompt, ids)
            self.paths, self.proposed = {"": []}, {}
        return self._prompt[1]

    def path(self, output):
        """Токены output: путь, который модель сама написала, иначе токенизация."""
        if output in self.paths:
            return self.paths[output]
        for k in range(1, min(len(output), 256) + 1):
            base, piece = output[:-k], output[-k:]
            if base in self.paths and piece in self.proposed.get(base, {}):
                path = self.paths[base] + [self.proposed[base][piece]]
                break
        else:
            path = self.tok.encode(output, self.names.copy())
        self.paths[output] = path
        return path

    def names_after(self, path):
        names = self.names.copy()
        self.tok.decode(path, names)
        return names

    def allowed(self, probs, names, end=True):
        """Распределение без служебных токенов, лишних переменных и байтов."""
        rest = probs.clone()
        rest[self.special] = 0
        if not end:
            rest[self.tok.eos] = 0
        rest[self.number > len(names.names)] = 0
        rest[self.bytes] = 0
        return rest

    def next(self, prompt, output, top):
        probs = self.distribution(self.ids(prompt, output), len(self.prompt_ids(prompt)))
        names = self.names_after(self.path(output))
        rest = self.allowed(probs, names, end=False)
        values, indices = torch.topk(rest, k=max(1, min(top, rest.numel())))
        candidates, proposed = [], self.proposed.setdefault(output, {})
        for v, i in zip(values.tolist(), indices.tolist()):
            if v > 0:
                text = self.tok.piece(int(i), names.copy())
                proposed.setdefault(text, int(i))
                candidates.append({"text": text, "p": float(v)})
        return {"candidates": candidates, "end": float(probs[self.tok.eos])}

    def score(self, prompt, output, continuations):
        """Оценки продолжений целиком: сумма log p их токенов после prompt+output."""
        base, start = self.ids(prompt, output), len(self.prompt_ids(prompt))
        names = self.names_after(self.path(output))
        scores = []
        for text in continuations:
            ids, seq, logp = self.tok.encode(text, names.copy()), list(base), 0.0
            for token in ids:
                logp += math.log(max(float(self.distribution(seq, start)[token]), 1e-45))
                seq.append(token)
            scores.append({"logp": logp, "tokens": len(ids)})
        return scores

    def raw(self, prompt, max_new_tokens=1024, prefix=""):
        """Сырой вывод: жадный, до <eos>. prefix — начало ответа, заданное
        заранее (каноническая запись): модель продолжает после него."""
        ids = list(self.prompt_ids(prompt))
        start, out, names = len(ids), [], self.names.copy()
        ids += self.tok.encode(prefix, names) if prefix else []
        after = names.copy()
        for _ in range(max_new_tokens):
            token = int(torch.argmax(self.allowed(self.distribution(ids, start), names)))
            if token == self.tok.eos:
                break
            self.tok.piece(token, names)
            ids.append(token)
            out.append(token)
        return prefix + self.tok.decode(out, after)
