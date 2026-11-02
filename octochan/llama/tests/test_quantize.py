"""Квантование своей модели: int8 и int4, размер весов по всем шардам."""

import os
import shutil
import tempfile
import unittest

import torch

from util import tiny_model

import quantize


class Quantize(unittest.TestCase):
    def test_roundtrip(self):
        torch.manual_seed(1)
        w = torch.randn(8, 64)
        for bits, group, tol in ((8, 0, 0.02), (4, 32, 0.25)):
            q, scale = quantize.quantize(w, bits, group)
            if bits == 4:
                q = quantize.unpack4(quantize.pack4(q), q.shape)
            err = (quantize.dequantize(q, scale) - w).abs().max().item()
            self.assertLess(err, tol * w.abs().max().item(), (bits, err))

    def test_group_must_divide(self):
        with self.assertRaises(ValueError):
            quantize.quantize(torch.randn(4, 48), 4, 32)

    def test_size_counts_shards(self):
        d = tempfile.mkdtemp()
        try:
            for name, size in (("model-00001-of-00002.safetensors", 100), ("model-00002-of-00002.safetensors", 50),
                               ("model.safetensors.index.json", 7), ("config.json", 9)):
                with open(os.path.join(d, name), "wb") as f:
                    f.write(b"x" * size)
            self.assertEqual(quantize.weights_size(d), 150)
        finally:
            shutil.rmtree(d)

    def test_quantized_model_agrees(self):
        src, out = tiny_model(), tempfile.mkdtemp()
        try:
            quantize.save(src, out, 8, 0)
            self.assertTrue(quantize.is_quantized(out))
            from transformers import LlamaForCausalLM

            base = LlamaForCausalLM.from_pretrained(src, dtype=torch.float32).eval()
            other = quantize.load(out).eval()
            ids = torch.tensor([[1, 5, 9, 13, 17]])
            with torch.no_grad():
                a, b = base(input_ids=ids).logits, other(input_ids=ids).logits
            # Веса случайные, логиты почти ровные: сверяется близость, а не
            # argmax — на обученной модели его сверяет quantize.py --check.
            self.assertLess((a - b).abs().max().item(), 0.05 * a.abs().max().item() + 1e-3)
            self.assertLess(quantize.weights_size(out), quantize.weights_size(src))
        finally:
            shutil.rmtree(src, ignore_errors=True)
            shutil.rmtree(out, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
