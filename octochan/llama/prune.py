"""Обрезка выходного словаря модели до алфавита ℒ — шаг 2.

Модель может написать только токены, из которых складывается текст
формального промпта: слова и знаки алфавита (ochan fp alphabet), их
сочетания из корпуса ℒ, отдельные буквы и цифры (имена объектов в языке
открыты) и конец ответа. Остальные строки выходного слоя удаляются: токена
кириллицы, эмодзи или «Sure» у модели больше нет.

    ochan fp alphabet > alphabet.tsv
    python prune.py --model HuggingFaceTB/SmolLM2-135M-Instruct --alphabet alphabet.tsv \
        --corpus ../examples/fp/connections.fp --out G:/fpm-llm/models/smollm2-135m-fpm
"""

import argparse
import glob
import json
import re
import string

import torch
from transformers import AutoModelForCausalLM, AutoTokenizer

import fpm
from adapter import answer_ids

# Знаки, которые встречаются в тексте ℒ: буквы, цифры, пробел, перевод
# строки и знаки грамматики. '#' — комментарий, то есть естественный язык,
# его в тексте модели нет.
CHARS = set(string.ascii_letters + string.digits + " \n_\"") | set("-->:=?.,(){}<@/%")
COMMENT = re.compile(r"#.*$", re.M)


def allowed(text):
    """Токен ℒ: знаки языка, и после \\n в токене ничего нет — утверждение
    (строка) складывается из целых токенов (adapter.answer_ids)."""
    return text != "" and all(c in CHARS for c in text) and "\n" not in text[:-1]


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--model", required=True)
    ap.add_argument("--alphabet", required=True, help="вывод ochan fp alphabet")
    ap.add_argument("--corpus", nargs="*", default=[], help="тексты ℒ: из них берутся сочетания токенов")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    tok = AutoTokenizer.from_pretrained(args.model)
    model = AutoModelForCausalLM.from_pretrained(args.model, dtype=torch.bfloat16)

    texts = set()
    with open(args.alphabet, encoding="utf-8") as f:
        for line in f:
            kind, _, item = line.rstrip("\n").partition("\t")
            if not item:
                continue
            # С заглавной в языке пишутся только классы: смыслы слов и имена
            # классов, составленные из слов (Reload, Config). Служебным словам
            # и ключевым заглавная не нужна — иначе в алфавит вошли бы The и To.
            variants = (item, item.capitalize()) if kind in ("word", "sense") else (item,)
            for variant in variants:
                texts.update({variant, " " + variant, "\n" + variant, "  " + variant})
    texts.update(CHARS)
    texts.update(" " * n for n in range(1, 9))
    texts.update("\n" + " " * n for n in range(0, 9))
    texts.add("\n\n")
    for pattern in args.corpus:
        for path in sorted(glob.glob(pattern)):
            with open(path, encoding="utf-8") as f:
                if path.endswith(".jsonl"):
                    # Корпус corpus.py: задачи и ответы.
                    for row in map(json.loads, f):
                        texts.update({row["task"], row["answer"]})
                else:
                    texts.add(COMMENT.sub("", f.read()))

    ids = set()
    for text in texts:
        for i in answer_ids(tok, text):
            if allowed(tok.decode([i], clean_up_tokenization_spaces=False)):
                ids.add(i)
    eos = model.generation_config.eos_token_id
    end = sorted({i for i in (eos if isinstance(eos, list) else [eos]) + [tok.eos_token_id] if i is not None})
    alphabet = sorted(ids) + [i for i in end if i not in ids]

    weight = model.get_output_embeddings().weight.detach()
    head = weight[torch.tensor(alphabet)]
    fpm.save(args.out, args.model, alphabet, end, head)

    outside = [i for i in alphabet if i not in end and not allowed(tok.decode([i], clean_up_tokenization_spaces=False))]
    vocab, hidden = weight.shape
    print(f"Словарь вывода: {vocab} -> {len(alphabet)} токенов (из них конец ответа: {len(end)})")
    print(f"Выходной слой: {vocab * hidden / 1e6:.1f} млн -> {len(alphabet) * hidden / 1e6:.2f} млн параметров")
    print(f"Токенов вне алфавита ℒ в выходе: {len(outside)}")
    print(f"Сохранено: {args.out}")


if __name__ == "__main__":
    main()
