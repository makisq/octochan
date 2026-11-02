"""Общее для тестов своей модели: крошечная модель со случайными весами.

Логика своей модели пишет копию задачи и выводимое сама, сеть выбирает
только порядок среди допустимых. Поэтому адаптер, квантование и обучение
проверяются на модели без обучения: ответ от весов почти не зависит.

alphabet.tsv — выгрузка ochan fp alphabet (базовый словарь, без скиллов):
    ochan fp alphabet > llama/tests/alphabet.tsv
"""

import os
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))

import torch  # noqa: E402
from transformers import LlamaForCausalLM  # noqa: E402

import fpmtok  # noqa: E402
import own  # noqa: E402

TASKS = os.path.join(os.path.dirname(HERE), "tasks")


def alphabet():
    with open(os.path.join(HERE, "alphabet.tsv"), encoding="utf-8") as f:
        return f.read()


def task(name):
    with open(os.path.join(TASKS, name), encoding="utf-8") as f:
        return f.read()


def tiny_model(directory=None, seed=0):
    """Каталог своей модели ℒ: словарь из алфавита, веса — случайные."""
    directory = directory or tempfile.mkdtemp(prefix="own-tiny-")
    torch.manual_seed(seed)
    tok = fpmtok.Tokenizer.build(alphabet())
    model = LlamaForCausalLM(own.config(tok, "tiny", 1024))
    model.save_pretrained(directory)
    tok.save(directory)
    return directory


def prompt(text):
    return "protocol\n\n# Task\n\n" + text
