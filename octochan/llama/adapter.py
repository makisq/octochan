"""Адаптер своей модели к протоколу движка ochan v1.

ochan сам ведёт генерацию во встроенном режиме (llm.mode: embedded): кольцо 0,
маска языка, откаты к началу утверждения. Адаптеру остаётся одно — отдавать
распределение следующего токена:

    GET  /v1/info  -> {"protocol": 1, "engine": "transformers", "model": "..."}
    POST /v1/next  <- {"prompt": "...", "output": "...", "top": 2}
                   -> {"candidates": [{"text": "goal", "p": 0.81}, ...], "end": 0.001}

p — вероятность в полном распределении модели, после softmax и до любой
выборки: температуры, top-p и штрафов здесь нет. end — вероятность конца
ответа (EOS и конец реплики). Кандидаты — по убыванию p, служебные токены
среди них не бывают.

prompt — неизменная часть: протокол, затем "\n\n# Task\n\n" и задача. У
instruct-модели она раскладывается по шаблону чата: system — протокол, user —
задача; output — начало ответа ассистента. Кеш KV держит общий префикс
прошлого запроса, поэтому досчитываются только новые токены.

    python adapter.py serve --model HuggingFaceTB/SmolLM2-135M-Instruct --listen 127.0.0.1:8090
    python adapter.py raw --model ... --prompt-file prompt.txt   # сырой вывод, без колец
"""

import argparse
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

import torch
from transformers import DynamicCache

import fpm

PROTOCOL = 1
TASK_MARKER = "\n\n# Task\n\n"
# Дата в системной реплике шаблона Llama 3: без неё шаблон берёт сегодняшнюю,
# и один и тот же промпт давал бы разные токены в разные дни.
DATE_STRING = "26 Jul 2024"


class Engine:
    def __init__(self, model, device="cuda", dtype="bfloat16", chat=True):
        self.name = model
        self.device = device
        # Каталог в ФПм-виде (fpm.json) — модель с выходом, обрезанным до ℒ:
        # номер выхода — номер в алфавите, а не идентификатор токена.
        self.model, self.tok, alphabet = fpm.load(model, device, getattr(torch, dtype))
        self.to_id = alphabet.tolist() if alphabet is not None else None
        self.chat = chat and self.tok.chat_template is not None

        eos = self.model.generation_config.eos_token_id
        eos = eos if isinstance(eos, list) else [eos]
        self.end_ids = sorted({i for i in eos + [self.tok.eos_token_id] if i is not None})
        special = set(self.tok.all_special_ids) | set(self.end_ids)
        vocab = self.model.get_output_embeddings().weight.shape[0]
        outputs = {t: k for k, t in enumerate(self.to_id)} if self.to_id else {i: i for i in range(vocab)}
        self.end_out = [outputs[i] for i in self.end_ids if i in outputs]
        self.special = torch.tensor(sorted(outputs[i] for i in special if i in outputs), device=device)
        self.outputs = vocab
        self._pieces = {}
        self._prompt = (None, None)
        self.cache, self.cache_ids = None, []

    def piece(self, token_id):
        if token_id not in self._pieces:
            self._pieces[token_id] = self.tok.decode([token_id], clean_up_tokenization_spaces=False)
        return self._pieces[token_id]

    def prompt_ids(self, prompt):
        if self._prompt[0] == prompt:
            return self._prompt[1]
        if self.chat:
            system, sep, task = prompt.partition(TASK_MARKER)
            messages = [{"role": "system", "content": system}]
            if sep:
                messages.append({"role": "user", "content": task})
            text = self.tok.apply_chat_template(messages, tokenize=False, add_generation_prompt=True,
                                                date_string=DATE_STRING)
            ids = self.tok(text, add_special_tokens=False).input_ids
        else:
            ids = self.tok(prompt).input_ids
        self._prompt = (prompt, ids)
        return ids

    def ids(self, prompt, output):
        ids = list(self.prompt_ids(prompt))
        if output:
            ids += self.tok(output, add_special_tokens=False).input_ids
        return ids

    @torch.inference_mode()
    def distribution(self, ids):
        """Распределение следующего токена после ids, с кешем общего префикса."""
        common = 0
        for a, b in zip(self.cache_ids, ids):
            if a != b:
                break
            common += 1
        # Хотя бы один токен считается заново: логиты нужны после последнего.
        common = min(common, len(ids) - 1)
        if self.cache is None or common == 0:
            self.cache, common = DynamicCache(), 0
        elif self.cache.get_seq_length() > common:
            # transformers 5: crop снимает с конца столько токенов, сколько сказано.
            self.cache.crop(common - self.cache.get_seq_length())
        out = self.model(input_ids=torch.tensor([ids[common:]], device=self.device),
                         past_key_values=self.cache, use_cache=True)
        self.cache, self.cache_ids = out.past_key_values, list(ids)
        return torch.softmax(out.logits[0, -1].float(), dim=-1)

    def token(self, output):
        """Идентификатор токена по номеру выхода модели."""
        return self.to_id[output] if self.to_id else output

    def next(self, prompt, output, top):
        probs = self.distribution(self.ids(prompt, output))
        end = float(probs[self.end_out].sum())
        rest = probs.clone()
        if self.special.numel():
            rest[self.special] = 0
        values, indices = torch.topk(rest, k=max(1, min(top, rest.numel())))
        candidates = [{"text": self.piece(self.token(int(i))), "p": float(v)}
                      for v, i in zip(values.tolist(), indices.tolist()) if v > 0]
        return {"candidates": candidates, "end": end}

    def raw(self, prompt, max_new_tokens=512):
        """Сырой вывод модели без колец и маски: жадный, до конца реплики."""
        ids, out = list(self.prompt_ids(prompt)), []
        for _ in range(max_new_tokens):
            token = self.token(int(torch.argmax(self.distribution(ids))))
            if token in self.end_ids:
                break
            ids.append(token)
            out.append(token)
        return self.tok.decode(out, skip_special_tokens=True, clean_up_tokenization_spaces=False)


def serve(engine, listen):
    host, port = listen.rsplit(":", 1)

    class Handler(BaseHTTPRequestHandler):
        def reply(self, code, body):
            data = json.dumps(body, ensure_ascii=False).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            if self.path != "/v1/info":
                return self.reply(404, {"error": "нет такого адреса"})
            self.reply(200, {"protocol": PROTOCOL, "engine": "transformers", "model": engine.name})

        def do_POST(self):
            if self.path != "/v1/next":
                return self.reply(404, {"error": "нет такого адреса"})
            try:
                req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
                self.reply(200, engine.next(req["prompt"], req.get("output", ""), int(req.get("top", 2))))
            except Exception as e:  # ochan показывает текст ошибки движка как есть
                self.reply(500, {"error": f"{type(e).__name__}: {e}"})

        def log_message(self, *args):
            pass

    print(f"Адаптер: http://{listen}, протокол v{PROTOCOL}, модель {engine.name}, "
          f"шаблон чата: {'да' if engine.chat else 'нет'}", flush=True)
    HTTPServer((host, int(port)), Handler).serve_forever()


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("serve", "raw"):
        p = sub.add_parser(name)
        p.add_argument("--model", required=True, help="каталог или имя модели на Hugging Face")
        p.add_argument("--device", default="cuda" if torch.cuda.is_available() else "cpu")
        p.add_argument("--dtype", default="bfloat16")
        p.add_argument("--no-chat", action="store_true", help="не оборачивать prompt шаблоном чата")
    sub.choices["serve"].add_argument("--listen", default="127.0.0.1:8090")
    sub.choices["raw"].add_argument("--prompt-file", required=True)
    sub.choices["raw"].add_argument("--max-new-tokens", type=int, default=512)
    args = ap.parse_args()

    engine = Engine(args.model, args.device, args.dtype, chat=not args.no_chat)
    if args.cmd == "serve":
        serve(engine, args.listen)
        return
    with open(args.prompt_file, encoding="utf-8") as f:
        prompt = f.read()
    started = time.time()
    text = engine.raw(prompt, args.max_new_tokens)
    sys.stdout.write(text)
    print(f"\n[{time.time() - started:.1f} с]", file=sys.stderr)


if __name__ == "__main__":
    main()
