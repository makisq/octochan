"""Обучение своей модели на процессоре: без видеокарты оно идёт так же."""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

from util import HERE, alphabet, task

import corpus

LLAMA = os.path.dirname(HERE)


class TrainOnCPU(unittest.TestCase):
    def test_one_step(self):
        d = tempfile.mkdtemp()
        try:
            rows = []
            for name in ("pg_memory.fp", "disk_cleanup.fp", "nginx_reload.fp"):
                text = task(name)
                answer = corpus.solve(text)
                if answer:
                    rows.append({"task": text, "answer": answer, "split": "train"})
            rows.append(dict(rows[0], split="test"))
            path = os.path.join(d, "corpus.jsonl")
            with open(path, "w", encoding="utf-8") as f:
                f.writelines(json.dumps(r, ensure_ascii=False) + "\n" for r in rows)
            with open(os.path.join(d, "alphabet.tsv"), "w", encoding="utf-8") as f:
                f.write(alphabet())
            out = os.path.join(d, "model")
            run = subprocess.run([sys.executable, os.path.join(LLAMA, "train_own.py"), "--alphabet",
                                  os.path.join(d, "alphabet.tsv"), "--corpus", path, "--out", out, "--size", "tiny",
                                  "--epochs", "1", "--device", "cpu"],
                                 capture_output=True, text=True, timeout=900, cwd=LLAMA)
            self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
            self.assertIn("Сохранено", run.stdout)
            self.assertTrue(os.path.isfile(os.path.join(out, "fpmtok.json")))
        finally:
            shutil.rmtree(d, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
