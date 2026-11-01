"""Токенизатор своей модели: словарь — алфавит ℒ, частные имена — переменные.

Статистика модели — только по конечному словарю языка: слова словаря и их
смыслы-классы, служебные и ключевые слова, единицы, знаки грамматики
(ochan fp alphabet), цифры и разметка строки. Составное имя со смыслом
(connection_limit) — слова через «_».

Всё прочее — имена частных случаев: значения слотов (o_17, plod_ser2222_ru),
имена стрелок-частных случаев, ячеек, версий, путей binds_to. Их смысл задаёт
не словарь, а место в структуре: стрелка instance связывает значение со
слотом обобщения. Поэтому имя частного случая — переменная <vK>, K — номер его
первого появления в тексте (задача, затем ответ). Модель не учит, что такое
plod_ser2222_ru, а учит, куда переносится переменная; ответ по построению не
меняется, когда меняется только имя (критерий А.6 манифеста, пункт а).

Текст сначала приводится к канонической записи (canonical): без
комментариев, одиночные пробелы внутри строки, отступ — по два пробела,
не больше одной пустой строки подряд. У каждого токена есть форма с пробелом
впереди (« GB», « set», « <v3>»), у слов и цифр — с «_» впереди («_nightly»):
отдельный пробел или «_» между токенами модели пришлось бы перешагивать при
копировании, а их в тексте сотни. Маленькая модель на таком словаре не
копировала единицы после чисел (8 GB → 8 m) и склеивала куски разных
составных имён (low_nightly → low_quota). Словарь без этих форм (первый
вариант) читается по-прежнему: формы берутся, только если они в словаре.

Новое имя, которого не было в задаче (ячейка с решениями), модель пишет
следующей свободной переменной; на письме это cK со свободным K — так ячейки
называет и corpus.solve.
"""

import json
import os
import re

FILE = "fpmtok.json"
PAD, BOS, ANSWER, EOS = "<pad>", "<bos>", "<answer>", "<eos>"
SPECIAL = [PAD, BOS, ANSWER, EOS]
LAYOUT = ["\n", "  ", " "]
DIGITS = list("0123456789")
# «_» склеивает составное имя; n — приставка ??n_ (a_ — служебное слово a).
EXTRA = ['"', "_", "n"]
MAX_VARIABLES = 64

COMMENT = re.compile(r'^((?:[^"#]|"[^"]*")*)#.*$')
IDENT = re.compile(r"[^\W\d]\w*")
VARIABLE = re.compile(r"( ?)<v(\d+)>")
BYTE = re.compile(r"<0x([0-9A-F]{2})>")


def canonical(text):
    """Каноническая запись текста на ℒ."""
    lines = []
    for line in text.replace("\r", "").split("\n"):
        line = COMMENT.sub(r"\1", line).rstrip()
        body = line.lstrip(" ")
        if not body:
            lines.append("")
            continue
        lines.append(" " * (len(line) - len(body)) + re.sub(r" {2,}", " ", body))
    text = re.sub(r"\n{3,}", "\n\n", "\n".join(lines).strip("\n"))
    return text + "\n" if text else ""


class Names:
    """Имена частных случаев одного текста: номер переменной ↔ имя."""

    def __init__(self, names=()):
        self.names = list(names)
        self.index = {n: k for k, n in enumerate(self.names)}

    def copy(self):
        return Names(self.names)

    def of(self, name):
        """Номер переменной имени; новое имя — следующий номер."""
        if name not in self.index:
            self.index[name] = len(self.names)
            self.names.append(name)
        return self.index[name]

    def name(self, k):
        """Имя переменной k; следующая свободная — новое имя cK."""
        if k == len(self.names):
            fresh = next(f"c{n}" for n in range(1, 10000) if f"c{n}" not in self.index)
            self.of(fresh)
        return self.names[k]


class Tokenizer:
    def __init__(self, tokens):
        self.tokens = tokens
        self.ids = {t: i for i, t in enumerate(tokens)}
        self.pad, self.bos, self.answer, self.eos = (self.ids[t] for t in SPECIAL)
        # Переменная: токен → (пробел впереди или нет, номер).
        self.variable = {i: (m.group(1), int(m.group(2))) for i, t in enumerate(tokens) if (m := VARIABLE.fullmatch(t))}
        self.byte = {i: int(m.group(1), 16) for i, t in enumerate(tokens) if (m := BYTE.fullmatch(t))}
        self.closed = {t for i, t in enumerate(tokens)
                       if t not in SPECIAL and i not in self.variable and i not in self.byte}
        self.puncts = sorted((t for t in self.closed if not IDENT.fullmatch(t) and t not in LAYOUT
                              and t not in DIGITS and not t.startswith((" ", "_"))), key=len, reverse=True)

    @classmethod
    def build(cls, alphabet, forms=True):
        """Словарь из ochan fp alphabet (строки «вид<TAB>текст»); forms — формы
        токенов с пробелом и «_» впереди."""
        words = []
        for line in alphabet.splitlines():
            if "\t" in line:
                text = line.split("\t", 1)[1]
                if text and text not in words:
                    words.append(text)
        base = DIGITS + [t for t in EXTRA if t not in words] + words
        variables = [f"<v{k}>" for k in range(MAX_VARIABLES)]
        tokens = SPECIAL + LAYOUT + base
        if forms:
            tokens += [" " + t for t in base if t != "_"]
            tokens += ["_" + t for t in base if t != "_" and (IDENT.fullmatch(t) or t.isdigit())]
            variables += [" " + v for v in variables]
        tokens += variables + [f"<0x{b:02X}>" for b in range(256)]
        return cls(tokens)

    @classmethod
    def load(cls, path):
        with open(os.path.join(path, FILE), encoding="utf-8") as f:
            return cls(json.load(f)["tokens"])

    def save(self, path):
        os.makedirs(path, exist_ok=True)
        with open(os.path.join(path, FILE), "w", encoding="utf-8") as f:
            json.dump({"tokens": self.tokens}, f, ensure_ascii=False, indent=0)

    def __len__(self):
        return len(self.tokens)

    def name_texts(self, names, name):
        k = names.of(name)
        if k < MAX_VARIABLES:
            return [f"<v{k}>"]
        return [f"<0x{b:02X}>" for b in name.encode("utf-8")]

    def ident(self, word, names):
        """Слово словаря, составное имя из слов — или переменная."""
        if word in self.closed:
            return [word]
        parts = word.split("_")
        if all(p == "" or p in self.closed or p.isdigit() for p in parts):
            out = []
            for k, p in enumerate(parts):
                if k:
                    out.append("_")
                out += list(p) if p.isdigit() else ([p] if p else [])
            return out
        return self.name_texts(names, word)

    def texts(self, text, names):
        """Лексемы канонического текста: тексты токенов без форм, переменная — <vK>."""
        out = []
        for n, line in enumerate(text.split("\n")):
            if n:
                out.append("\n")
            body = line.lstrip(" ")
            indent = len(line) - len(body)
            out += ["  "] * (indent // 2) + [" "] * (indent % 2)
            if body.startswith("binds_to:"):
                # Путь к коду — одно имя частного случая.
                path = body[len("binds_to:"):].strip()
                out += ["binds_to", ":"]
                if path:
                    out += [" "] + self.name_texts(names, path)
                continue
            i = 0
            while i < len(body):
                c = body[i]
                if c == " " or c.isdigit():
                    out.append(c)
                    i += 1
                elif m := IDENT.match(body, i):
                    out += self.ident(m.group(), names)
                    i = m.end()
                else:
                    p = next((p for p in self.puncts if body.startswith(p, i)), None)
                    if p:
                        out.append(p)
                        i += len(p)
                    else:
                        out += [f"<0x{b:02X}>" for b in c.encode("utf-8")]
                        i += 1
        return out

    def encode(self, text, names):
        """Токены текста, уже приведённого к канонической записи: пробел и «_»
        входят в следующий токен, если такая форма есть в словаре."""
        texts, out, i = self.texts(text, names), [], 0
        while i < len(texts):
            t = texts[i]
            if t in (" ", "_") and i + 1 < len(texts) and t + texts[i + 1] in self.ids:
                out.append(self.ids[t + texts[i + 1]])
                i += 2
                continue
            out.append(self.ids[t])
            i += 1
        return out

    def piece(self, token, names):
        """Текст одного токена; переменная — имя (новое — cK)."""
        if token in self.variable:
            space, k = self.variable[token]
            return space + names.name(k)
        if token in self.byte:
            return bytes([self.byte[token]]).decode("utf-8", errors="replace")
        text = self.tokens[token]
        return "" if text in SPECIAL else text

    def decode(self, ids, names):
        out, raw = [], b""
        for t in ids:
            if t in self.byte:
                raw += bytes([self.byte[t]])
                continue
            if raw:
                out.append(raw.decode("utf-8", errors="replace"))
                raw = b""
            out.append(self.piece(t, names))
        if raw:
            out.append(raw.decode("utf-8", errors="replace"))
        return "".join(out)

    def pair(self, task, answer=None):
        """Последовательность модели: <bos> задача <answer> [ответ <eos>].
        Возвращает токены, длину промпта и имена."""
        names = Names()
        ids = [self.bos] + self.encode(canonical(task), names) + [self.answer]
        start = len(ids)
        if answer is not None:
            ids += self.encode(canonical(answer), names) + [self.eos]
        return ids, start, names
