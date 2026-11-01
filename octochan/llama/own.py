"""Своя модель ℒ — архитектурная: сеть, словарь, логика и кольца — одна вещь.

Модель не дообучается из чужой: её веса и словарь — только ℒ. Протокол она
не читает — язык у неё в весах; из prompt ochan берётся только задача (после
TASK_MARKER) в канонической записи, имена частных случаев — переменные.

Всё, что в манифесте сказано о кольцах и выводе, у неё в коде, а не в
протоколе вокруг (протокольный подход — для внешних моделей):

- задачу без предпосылки (logic.missing) модель не решает: отказ —
  её собственный ответ;
- копию задачи и то, что следует из посылок, пишет её логика (logic.py);
  кольцо 1 даёт поддержку выведенному по надёжности правил из журнала,
  кольцо 2 не пропускает слабое — оно уходит вопросом оператору (rings.py);
- задачу, структура которой повторяет кластер инцидентов, модель узнаёт
  сама (logic.shape): надёжность правил на ней — по исходам его инцидентов
  (learn.py), а плохо решённая структура — вопрос оператору;
- сеть выбирает только среди допустимых: правилом выбора с весами и
  калибровкой из журнала (rings.json рядом с весами);
- её функция распределения переопределена: следующий токен вне допустимых
  логикой утверждений имеет вероятность ноль, единственный допустимый —
  единицу. Это видит и протокол движка v1 (/v1/next).

Режим у модели один — этот: ochan её по токенам не ведёт (декодер ochan —
для чужих моделей). generate — ответ модели целиком (/v1/generate); raw —
сеть одна, без логики и колец: так меряется, что даёт архитектура, но это
замер, а не режим работы.

    python adapter.py serve --model G:/fpm-llm/models/own-small-2 --listen 127.0.0.1:8093
"""

import math
import os

import torch
from transformers import LlamaConfig, LlamaForCausalLM

import fpmtok
import logic
import rings
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


def narrowings(params, plan):
    """Сужения неточных предпосылок по истории из журнала (раздел 5.2):
    прочтение — значение, которое оператор задавал атрибуту точно или
    одобрил в сужении; поддержка — доля прочтения среди всех. Лидер сужает,
    только если его нижняя граница выше верхних границ остальных; иначе
    варианты сопоставимы, и решает оператор."""
    derived = {(d.target, d.attr) for d in plan.decisions}
    decided = {d.attr: d.value for d in plan.decisions}
    out = []
    for (target, attr), vague in plan.task.inexact.items():
        counts = params.history.get(attr, {})
        total = sum(counts.values())
        if (target, attr) in derived or not total:
            continue
        candidates = vague.variants if vague.variants is not None else [v for v in counts if vague.admits(v)]
        if not candidates:
            continue
        spans = {v: rings.share(counts.get(v, 0), total) for v in candidates}
        leader = max(candidates, key=lambda v: spans[v][0])
        rest = [spans[v][1] for v in candidates if v != leader]
        if vague.variants is None:
            # Другие числа диапазона — всё, что не лидер.
            rest.append(rings.share(total - counts.get(leader, 0), total)[1])
        if rest and spans[leader][0] <= max(rest) or decided.get(attr, leader) != leader:
            continue
        names = vague.variants if vague.variants and all(logic.NAME.match(v) for v in vague.variants) else None
        out.append((logic.Decision(target, attr, leader, f"history({attr})", ["fp.history"], names), spans[leader]))
    return out


class Engine(Base):
    # Ответ на задачу пишет целиком сама: адаптер отдаёт /v1/generate.
    architectural = True

    def __init__(self, model, device="cuda", dtype="float32"):
        self.name = model
        self.device = device
        self.params = rings.Params.load(model)
        self._plans, self._copies = {}, {}
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

    def plan(self, task):
        """План ответа после колец 1 и 2: (план, недостающее, пакеты).
        Пакет — выведенное решение с правилами, поддержкой и вердиктом.
        Задача, структура которой повторяет кластер инцидентов (learn.py),
        берёт надёжность правил на этой структуре: plan.cluster — его ключ
        и параметры."""
        if task not in self._plans:
            plan = logic.AnswerPlan(task)
            plan.cluster = self.params.cluster_of(logic.shape(task, self.tok.closed.__contains__))
            key, cluster = plan.cluster
            packages, accepted = [], []
            narrowed = self.narrowings(plan)
            spans = {(d.target, d.attr): span for d, span in narrowed}
            plan.decisions = [d for d, _ in narrowed] + plan.decisions
            for d in plan.decisions:
                lower, upper = spans.get((d.target, d.attr)) or rings.support(self.params.rule(r, cluster) for r in d.rules)
                ok = lower >= self.params.well_true
                package = {"statement": d.line().strip(), "rules": d.rules, "l": lower, "u": upper,
                           "verdict": "well_true" if ok else "well_false"}
                if cluster and any(r in cluster["rules"] for r in d.rules):
                    package["cluster"] = key
                packages.append(package)
                if ok:
                    accepted.append(d)
            plan.decisions = accepted
            self._plans[task] = (plan, logic.missing(task), packages)
        return self._plans[task]

    def narrowings(self, plan):
        return narrowings(self.params, plan)

    def questions(self, plan, packages):
        """Вопросы оператору: что посылки не закрывают, что кольцо 2 не
        приняло и какую неточную предпосылку сузить не по чему."""
        out = [{"attr": q.attr, "targets": q.targets, "values": q.values, "why": "посылки не выбирают между значениями"}
               for q in plan.open]
        narrowed = {(d.target, d.attr) for d in plan.decisions}
        for (target, attr), vague in plan.task.inexact.items():
            if (target, attr) not in narrowed and not any(target in q.targets and q.attr == attr for q in plan.open):
                out.append({"statement": f"set {target}.{attr} = {vague.text}",
                            "why": "неточная предпосылка: сузить её нечем — ни посылки, ни истории"})
        key, cluster = getattr(plan, "cluster", (None, None))
        for p in packages:
            if p["verdict"] != "well_false":
                continue
            q = {"statement": p["statement"], "rules": p["rules"],
                 "why": f"правила {', '.join(p['rules'])} проверены мало: нижняя граница поддержки "
                        f"{p['l']:.2f} ниже порога {self.params.well_true:.2f}"}
            if p.get("cluster"):
                counts = "; ".join(f"{r} верно {cluster['rules'][r][0]:.1f} из {cluster['rules'][r][1]:.1f}"
                                   for r in p["rules"] if r in cluster["rules"])
                q.update(cluster=key, why=f"на инцидентах этой структуры (кластер {key}) {counts}: нижняя граница "
                                         f"поддержки {p['l']:.2f} ниже порога {self.params.well_true:.2f}")
            out.append(q)
        if cluster and cluster["outcomes"][1]:
            lower, _ = self.params.outcomes(cluster)
            if lower < self.params.well_true:
                solved, total = cluster["outcomes"]
                out.append({"cluster": key, "why": f"инциденты этой структуры (кластер {key}) модель решила сама "
                                                   f"{solved:.1f} из {total:.1f}: нижняя граница {lower:.2f} ниже порога "
                                                   f"{self.params.well_true:.2f} — решает оператор"})
        return out

    def cluster(self, plan):
        """Кластер инцидентов, структуру которого повторяет задача, — для
        ответа: ключ, частных случаев, исходы и надёжность модели на нём."""
        key, cluster = getattr(plan, "cluster", (None, None))
        if cluster is None:
            return None
        lower, upper = self.params.outcomes(cluster)
        return {"key": key, "instances": len(cluster["instances"]), "solved": cluster["outcomes"][0],
                "outcomes": cluster["outcomes"][1], "l": lower, "u": upper}

    def generate(self, prompt):
        """Ответ модели целиком: отказ без предпосылки, копия, пакеты логики
        после колец, выбор сети правилом выбора среди допустимых."""
        plan, missing, packages = self.plan(task_of(prompt))
        if missing:
            return {"answer": None, "refused": True, "missing": missing, "packages": [], "questions": [],
                    "forced": 0, "chosen": 0, "choices": []}
        by_line = {p["statement"]: p for p in packages}
        output, forced, choices = plan.copy, 0, []
        while True:
            adm, ok = plan.next(output)
            if not ok:
                raise RuntimeError("логика модели не ведёт её же ответ")
            if not adm:
                break
            if len(adm) == 1:
                output += adm[0]
                forced += 1
                continue
            ranked = []
            for line, s in zip(adm, self.score(prompt, output, adm)):
                p = by_line[line.strip()]
                mean = s["logp"] / max(1, s["tokens"])
                # G = 1: выведенное логикой целиком привязано к посылкам.
                ranked.append((self.params.score(mean, p["l"], p["u"], 1.0), mean, line))
            ranked.sort(key=lambda r: -r[0])
            best = ranked[0]
            by_line[best[2].strip()].update(chosen=True, score=best[0], logprob=best[1], options=len(adm),
                                            gap=best[0] - ranked[1][0])
            choices.append({"statement": best[2].strip(), "options": len(adm), "gap": best[0] - ranked[1][0]})
            output += best[2]
        return {"answer": output, "refused": False, "missing": [], "packages": packages,
                "questions": self.questions(plan, packages), "forced": forced, "chosen": len(choices),
                "choices": choices, "cluster": self.cluster(plan)}

    def admissible(self, task, output):
        """Токены, которыми может продолжиться output по логике модели:
        множество, "end" — ответ закончен, None — логика это место не ведёт."""
        plan, missing, _ = self.plan(task)
        if missing:
            return "end"
        if len(output) < len(plan.copy):
            if not plan.copy.startswith(output):
                return None
            if task not in self._copies:
                self._copies[task] = self.tok.encode(plan.copy, self.names.copy())
            lines, head, partial, ids_of = [plan.copy], "", output, {plan.copy: self._copies[task]}
        else:
            cut = output.rfind("\n") + 1
            head, partial = output[:cut], output[cut:]
            adm, ok = plan.next(head)
            if not ok:
                return None
            if not adm:
                return "end" if partial == "" else None
            lines = [line for line in adm if line.startswith(partial)]
            names = self.names_after(self.path(head))
            ids_of = {line: self.tok.encode(line, names.copy()) for line in lines}
        names = self.names_after(self.path(head)) if head else self.names.copy()
        tokens = set()
        for line in lines:
            ids, text, k = ids_of[line], "", 0
            while len(text) < len(partial) and k < len(ids):
                text += self.tok.piece(ids[k], names.copy())
                k += 1
            if text == partial and k < len(ids):
                tokens.add(ids[k])
        return tokens

    def next(self, prompt, output, top):
        self.prompt_ids(prompt)
        allowed = self.admissible(task_of(prompt), output)
        if allowed is None:
            return self.net_next(prompt, output, top)
        if allowed == "end" or not allowed:
            return {"candidates": [], "end": 1.0}
        names = self.names_after(self.path(output))
        proposed = self.proposed.setdefault(output, {})
        if len(allowed) == 1:
            token = next(iter(allowed))
            text = self.tok.piece(token, names.copy())
            proposed.setdefault(text, token)
            return {"candidates": [{"text": text, "p": 1.0}], "end": 0.0}
        probs = self.distribution(self.ids(prompt, output), len(self.prompt_ids(prompt)))
        index = torch.tensor(sorted(allowed), device=self.device)
        mass = probs[index]
        mass = mass / mass.sum() if float(mass.sum()) > 0 else torch.full_like(mass, 1 / len(index))
        order = torch.argsort(mass, descending=True)[:max(1, top)]
        candidates = []
        for k in order.tolist():
            token = int(index[k])
            text = self.tok.piece(token, names.copy())
            proposed.setdefault(text, token)
            candidates.append({"text": text, "p": float(mass[k])})
        return {"candidates": candidates, "end": 0.0}

    def net_next(self, prompt, output, top):
        """Распределение сети одной, без логики модели."""
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
