"""Обучение весов своей модели на процессоре: каталог и корпус — как их
пишет ochan (llm init, llm corpus --model), обучение — train_own.py.

Словарь, логика и кольца модели — в ochan; здесь проверяется только то,
что считает torch: веса учатся по токенам корпуса и сохраняются так, что
их читает ochan (config.json и model.safetensors)."""

import json
import os
import random
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(os.path.dirname(HERE), "train_own.py")


def model_dir(vocab=64):
    """Каталог модели, как его пишет ochan llm init: словарь и конфигурация."""
    d = tempfile.mkdtemp(prefix="own-train-")
    tokens = ["<pad>", "<bos>", "<answer>", "<eos>"] + [f"t{i}" for i in range(vocab - 4)]
    with open(os.path.join(d, "fpmtok.json"), "w", encoding="utf-8") as f:
        json.dump({"tokens": tokens}, f)
    config = {"architectures": ["LlamaForCausalLM"], "model_type": "llama", "hidden_act": "silu",
              "vocab_size": vocab, "hidden_size": 32, "num_hidden_layers": 2, "num_attention_heads": 2,
              "num_key_value_heads": 2, "intermediate_size": 64, "head_dim": 16, "max_position_embeddings": 128,
              "rms_norm_eps": 1e-6, "rope_theta": 10000.0, "tie_word_embeddings": True, "attention_bias": False,
              "mlp_bias": False, "bos_token_id": 1, "eos_token_id": 3, "pad_token_id": 0, "initializer_range": 0.02}
    with open(os.path.join(d, "config.json"), "w", encoding="utf-8") as f:
        json.dump(config, f)
    return d


def corpus(path, pairs=60, vocab=64, seed=1):
    """Корпус с токенами, как ochan llm corpus --model: задача — случайные
    токены, ответ — та же последовательность (копию учить просто)."""
    rng = random.Random(seed)
    with open(path, "w", encoding="utf-8") as f:
        for n in range(pairs):
            task = [rng.randrange(4, vocab) for _ in range(rng.randint(4, 10))]
            ids = [1] + task + [2] + task + [3]
            f.write(json.dumps({"task": "", "answer": "", "split": "test" if n % 10 == 0 else "train",
                                "ids": ids, "start": len(task) + 2}) + "\n")


class TrainOwn(unittest.TestCase):
    def test_trains_on_cpu(self):
        d = model_dir()
        data = os.path.join(d, "corpus.jsonl")
        corpus(data)
        out = subprocess.run([sys.executable, SCRIPT, "--model", d, "--corpus", data, "--epochs", "4",
                              "--batch-tokens", "512", "--lr", "3e-3", "--device", "cpu"],
                             capture_output=True, text=True, timeout=600)
        self.assertEqual(out.returncode, 0, out.stderr)
        lines = out.stdout.splitlines()
        before = next(l for l in lines if l.startswith("Потери на отложенных до обучения"))
        after = next(l for l in lines if l.startswith("Потери на отложенных после обучения"))
        loss = lambda line: float(line.split("пара ")[1].split(",")[0])
        self.assertLess(loss(after), loss(before), out.stdout)
        self.assertTrue(os.path.isfile(os.path.join(d, "model.safetensors")))
        with open(os.path.join(d, "config.json"), encoding="utf-8") as f:
            self.assertEqual(json.load(f)["vocab_size"], 64)

    def test_refuses_corpus_without_tokens(self):
        d = model_dir()
        data = os.path.join(d, "corpus.jsonl")
        with open(data, "w", encoding="utf-8") as f:
            f.write(json.dumps({"task": "goal: x\n", "answer": "goal: x\n", "split": "train"}) + "\n")
        out = subprocess.run([sys.executable, SCRIPT, "--model", d, "--corpus", data, "--device", "cpu"],
                             capture_output=True, text=True, timeout=120)
        self.assertNotEqual(out.returncode, 0)
        self.assertIn("нет токенов", out.stderr)

    def test_init_keeps_previous(self):
        d = model_dir()
        data = os.path.join(d, "corpus.jsonl")
        corpus(data, pairs=20)
        with open(os.path.join(d, "rings.json"), "w", encoding="utf-8") as f:
            f.write("{}")
        subprocess.run([sys.executable, SCRIPT, "--model", d, "--corpus", data, "--epochs", "1", "--device", "cpu"],
                       capture_output=True, text=True, timeout=600, check=True)
        new = d + "-2"
        out = subprocess.run([sys.executable, SCRIPT, "--init", d, "--model", new, "--corpus", data, "--epochs", "1",
                              "--device", "cpu"], capture_output=True, text=True, timeout=600)
        self.assertEqual(out.returncode, 0, out.stderr)
        for name in ("model.safetensors", "fpmtok.json", "rings.json"):
            self.assertTrue(os.path.isfile(os.path.join(new, name)), name)
        same = subprocess.run([sys.executable, SCRIPT, "--init", d, "--model", d, "--corpus", data],
                              capture_output=True, text=True, timeout=120)
        self.assertIn("новым каталогом", same.stderr)


if __name__ == "__main__":
    unittest.main()
