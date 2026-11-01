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
прошлого запроса, поэтому досчитываются только новые токены. Распределение —
функция prompt и output, а не порядка запросов: ответ всегда считается по
токену (distribution), и сырой вывод совпадает с жадным путём декодера.

Токен не переходит через перевод строки: декодер ochan проверяет язык по
строкам и обрезает токен на \n, а BPE пишет «\n  set» как «\n », « set».
Поэтому ответ токенизируется построчно (answer_ids) — так и при обучении, —
а токенов с текстом после \n в алфавите нет (prune.py). output, который
декодер продлил кандидатом этого же адаптера, кодируется путём, который
модель сама написала: повторная токенизация на стыке даёт другие токены
(«\n\n» одним вместо двух «\n»), и модель видела бы чужой путь.

Каталог своей модели ℒ (own.py: веса с нуля и fpmtok.json) адаптер узнаёт
сам: у неё свой словарь, и протокол ей не нужен.

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
# Длиннее токена в символах не бывает: дальше кандидата по концу output не ищут.
PIECE = 64


def prompt_ids(tok, prompt, chat=True):
    """Токены prompt ochan: у instruct-модели протокол — system, задача —
    user, затем начало реплики ассистента. Тот же вход видит обучение."""
    if not chat or tok.chat_template is None:
        return tok(prompt).input_ids
    system, sep, task = prompt.partition(TASK_MARKER)
    messages = [{"role": "system", "content": system}]
    if sep:
        messages.append({"role": "user", "content": task})
    text = tok.apply_chat_template(messages, tokenize=False, add_generation_prompt=True, date_string=DATE_STRING)
    return tok(text, add_special_tokens=False).input_ids


def lines(text):
    """Строки текста с их \\n; у последней \\n может не быть."""
    parts = text.split("\n")
    return [p + "\n" for p in parts[:-1]] + ([parts[-1]] if parts[-1] else [])


def answer_ids(tok, text):
    """Токены ответа построчно: ни один не переходит через перевод строки."""
    return [i for line in lines(text) for i in tok(line, add_special_tokens=False).input_ids]


def end_ids(model, tok):
    """Токены конца ответа: EOS модели и конец реплики."""
    eos = model.generation_config.eos_token_id
    eos = eos if isinstance(eos, list) else [eos]
    return sorted({i for i in eos + [tok.eos_token_id] if i is not None})


def turn_end(model, tok):
    """Токен, которым шаблон чата кончает реплику ассистента: <|eot_id|> у
    Llama 3, <|im_end|> у SmolLM2. Им кончается ответ при обучении."""
    for name in ("<|eot_id|>", "<|im_end|>", "<|end|>"):
        token = tok.convert_tokens_to_ids(name)
        if isinstance(token, int) and token != tok.unk_token_id and token in end_ids(model, tok):
            return token
    return end_ids(model, tok)[0]


class Engine:
    def __init__(self, model, device="cuda", dtype="bfloat16", chat=True):
        self.name = model
        self.device = device
        # Каталог в ФПм-виде (fpm.json) — модель с выходом, обрезанным до ℒ:
        # номер выхода — номер в алфавите, а не идентификатор токена.
        self.model, self.tok, alphabet = fpm.load(model, device, getattr(torch, dtype))
        self.to_id = alphabet.tolist() if alphabet is not None else None
        self.chat = chat and self.tok.chat_template is not None

        self.end_ids = end_ids(self.model, self.tok)
        special = set(self.tok.all_special_ids) | set(self.end_ids)
        vocab = self.model.get_output_embeddings().weight.shape[0]
        outputs = {t: k for k, t in enumerate(self.to_id)} if self.to_id else {i: i for i in range(vocab)}
        self.end_out = [outputs[i] for i in self.end_ids if i in outputs]
        self.special = torch.tensor(sorted(outputs[i] for i in special if i in outputs), device=device)
        self.outputs = vocab
        self._pieces = {}
        self._prompt = (None, None)
        self.cache, self.cache_ids = None, []
        # По prompt: путь токенов модели для каждого output и кандидаты,
        # предложенные после него ({текст: идентификатор}).
        self.paths, self.proposed = {"": []}, {}

    def piece(self, token_id):
        if token_id not in self._pieces:
            self._pieces[token_id] = self.tok.decode([token_id], clean_up_tokenization_spaces=False)
        return self._pieces[token_id]

    def prompt_ids(self, prompt):
        if self._prompt[0] != prompt:
            self._prompt = (prompt, prompt_ids(self.tok, prompt, self.chat))
            self.paths, self.proposed = {"": []}, {}
        return self._prompt[1]

    def path(self, output):
        """Токены output: прошлый путь и кандидат, которым декодер его продлил,
        иначе построчная токенизация."""
        if output in self.paths:
            return self.paths[output]
        for k in range(1, min(len(output), PIECE) + 1):
            base, piece = output[:-k], output[-k:]
            if base in self.paths and piece in self.proposed.get(base, {}):
                path = self.paths[base] + [self.proposed[base][piece]]
                break
        else:
            path = answer_ids(self.tok, output)
        self.paths[output] = path
        return path

    def ids(self, prompt, output):
        return list(self.prompt_ids(prompt)) + self.path(output)

    @torch.inference_mode()
    def distribution(self, ids, start):
        """Распределение следующего токена после ids (первые start — промпт),
        с кешем общего префикса.

        В bf16 итог зависит от того, как вход поделён на куски: тот же токен,
        посчитанный в куске и отдельно, даёт разные логиты, а на развилке
        p ≈ 0,5 это меняет выбор. Поэтому деление — функция ids, а не истории
        кеша: промпт без последнего токена — одним куском, дальше — по токену."""
        common = 0
        for a, b in zip(self.cache_ids, ids):
            if a != b:
                break
            common += 1
        head = start - 1
        if common < head:
            common = 0
        # Хотя бы один токен считается заново: логиты нужны после последнего.
        common = min(common, len(ids) - 1)
        if self.cache is None or common == 0:
            self.cache, common = DynamicCache(), 0
        elif self.cache.get_seq_length() > common:
            # transformers 5: crop снимает с конца столько токенов, сколько сказано.
            self.cache.crop(common - self.cache.get_seq_length())
        chunks = [ids[:head]] if common == 0 and head > 0 else []
        chunks += [[t] for t in ids[max(common, head):]]
        for chunk in chunks:
            out = self.model(input_ids=torch.tensor([chunk], device=self.device),
                             past_key_values=self.cache, use_cache=True)
            self.cache = out.past_key_values
        self.cache_ids = list(ids)
        return torch.softmax(out.logits[0, -1].float(), dim=-1)

    def token(self, output):
        """Идентификатор токена по номеру выхода модели."""
        return self.to_id[output] if self.to_id else output

    def next(self, prompt, output, top):
        probs = self.distribution(self.ids(prompt, output), len(self.prompt_ids(prompt)))
        end = float(probs[self.end_out].sum())
        rest = probs.clone()
        if self.special.numel():
            rest[self.special] = 0
        values, indices = torch.topk(rest, k=max(1, min(top, rest.numel())))
        candidates, proposed = [], self.proposed.setdefault(output, {})
        for v, i in zip(values.tolist(), indices.tolist()):
            if v > 0:
                token = self.token(int(i))
                proposed.setdefault(self.piece(token), token)
                candidates.append({"text": self.piece(token), "p": float(v)})
        return {"candidates": candidates, "end": end}

    def raw(self, prompt, max_new_tokens=512):
        """Сырой вывод модели без колец и маски: жадный, до конца реплики."""
        ids, out = list(self.prompt_ids(prompt)), []
        start = len(ids)
        for _ in range(max_new_tokens):
            token = self.token(int(torch.argmax(self.distribution(ids, start))))
            if token in self.end_ids:
                break
            ids.append(token)
            out.append(token)
        return self.tok.decode(out, skip_special_tokens=True, clean_up_tokenization_spaces=False)


def engine_for(model, device="cuda", dtype="bfloat16", chat=True):
    """Движок по каталогу модели: своя модель ℒ (own.py) или модель Hugging Face."""
    import own

    if own.is_own(model):
        return own.Engine(model, device)
    return Engine(model, device, dtype, chat)


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

    engine = engine_for(args.model, args.device, args.dtype, chat=not args.no_chat)
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
