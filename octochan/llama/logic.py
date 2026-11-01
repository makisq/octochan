"""Логика архитектурной модели ℒ — часть её кода, как веса.

Модель не угадывает то, что следует из точных посылок задачи: она это
выводит. Здесь — правила вывода (кольцо 1 с надёжностью правил из журнала,
rings.py) и множество допустимых утверждений ответа на каждом месте:

- отношение defines, limits или modifies переносит атрибут, который ячейка
  задаёт источнику, на цель, если цели он не задан (правило fp.<тип>);
- перенос — цепочкой: выведенное — посылка для следующего шага (A → B → C),
  правила решения — правила всего пути, и кольцо 1 перемножает их
  надёжность. Цель решается на первом шаге, на котором до неё дошёл
  перенос; другой путь — длиннее или по кругу — с другим значением, которое
  приоритет не снимает, делает её вопросом оператору, и от вопроса дальше
  ничего не переносится. Сужение по истории (own.py) — решение модели по
  статистике, а не вывод: дальше оно не переносится;
- два источника дают цели разные значения — решает приоритет (A < B:
  побеждает A); приоритет — точная посылка оператора, правило вывода то же
  — перенос; без приоритета — вопрос оператору;
- атрибут, заданный обобщению — схеме отношения, переходит на каждую его
  стрелку-частный случай, у которой его нет, кроме слотов (fp.arrow);
- значение, заданное задачей, не переписывается; в ячейке действует
  последняя запись;
- неточная предпосылка (set T.a = {x, y} или 100..500, раздел 5.2) сама
  ничего не переносит; её сужает точная посылка через отношение — внутри
  неё, с альтернативами-вариантами; значение вне — вопрос оператору.
  Сужение по истории из журнала делает модель (own.py), не логика задачи.

missing — недостающие предпосылки (⊥undef): с ними модель не отвечает.

Те же правила снаружи проверяет ochan (fp.Derive): там они — внешнее
кольцо, здесь — то, как модель устроена. Совпадение двух реализаций
проверяется на корпусах (fpcheck выдаёт ответ логики ochan).
"""

import re
from dataclasses import dataclass, field

from fpmtok import canonical

CARRYING = ("defines", "limits", "modifies")
BUILTINS = {"exec", "ochan", "save", "write"}

COMMENT = re.compile(r'^((?:[^"#]|"[^"]*")*)#.*$')
RELATION = re.compile(r"(\w+)\s+--(\w+)-->\s+(\w+)\s*(?:\((\w+)\))?$")
SET = re.compile(r"set\s+(\w+)\.(\w+)\s*=\s*(.+?)\s*$")
CELL = re.compile(r"cell\s+(\w+)\s*:")
GENERALIZATION = re.compile(r"generalization\s+(\w+)\s*\((.*)\)\s*:")
INSTANCE = re.compile(r"instance\s+(\w+)\s*=\s*(\w+)\s*\((.*)\)")
SLOT = re.compile(r"(\w+)\s*:(?!=)")
BINDING = re.compile(r"(\w+)\s*:=")
RULE = re.compile(r"(?:hard\s+(?:forbid|require)\s+|\?\?[na]_)(\w+)\s*\(")
OPERATION = re.compile(r"=\s*(\w+)\s*\(")
NAME = re.compile(r"[A-Za-z_]\w*$")
RANGE = re.compile(r"(\d+(?:\.\d+)?)\s*([A-Za-z%][\w/%]*)?\s*\.\.\s*(\d+(?:\.\d+)?)\s*([A-Za-z%][\w/%]*)?$")
NUMBER = re.compile(r"(\d+(?:\.\d+)?)\s*([A-Za-z%][\w/%]*)?$")


@dataclass
class Inexact:
    """Неточное значение: варианты или диапазон."""
    text: str
    variants: list = None
    lo: float = None
    hi: float = None
    unit: str = ""

    def admits(self, value):
        if self.variants is not None:
            return value in self.variants
        m = NUMBER.match(value)
        return bool(m) and (m.group(2) or "").lower() == self.unit.lower() and self.lo <= float(m.group(1)) <= self.hi


def inexact(text):
    """Неточное значение из текста правки или None — значение точное."""
    text = text.strip()
    if text.startswith("{") and text.endswith("}"):
        return Inexact(text, variants=[v.strip() for v in text[1:-1].split(",") if v.strip()])
    m = RANGE.match(text)
    if m:
        return Inexact(text, lo=float(m.group(1)), hi=float(m.group(3)), unit=m.group(4) or "")
    return None


@dataclass
class Task:
    """Разобранная задача: то, из чего выводятся решения."""
    relations: list = field(default_factory=list)   # (from, type, to, label | None)
    priorities: list = field(default_factory=list)  # (higher, lower)
    given: dict = field(default_factory=dict)       # (цель, атрибут) -> значение
    cells: list = field(default_factory=list)
    gens: dict = field(default_factory=dict)        # обобщение -> слоты
    instances: list = field(default_factory=list)   # (имя, обобщение, заполненные слоты)
    inexact: dict = field(default_factory=dict)     # (цель, атрибут) -> Inexact

    def exact(self, key):
        return key in self.given and key not in self.inexact


@dataclass
class Decision:
    """Решение, выведенное из посылок: set target.attr = value."""
    target: str
    attr: str
    value: str
    basis: str
    rules: list
    alternatives: list = None

    def line(self):
        return f"  set {self.target}.{self.attr} = {self.value}\n"


@dataclass
class Open:
    """Вопрос, который посылки не закрывают: решает оператор."""
    targets: list
    attr: str
    values: list


def parse(text):
    task, section = Task(), None
    for line in text.replace("\r", "").split("\n"):
        line = COMMENT.sub(r"\1", line).rstrip()
        if not line.strip():
            continue
        if not line[0].isspace():
            head = line.strip()
            section = head.split(":")[0].split()[0]
            if m := CELL.match(head):
                task.cells.append(m.group(1))
                section = "cell"
            elif m := GENERALIZATION.match(head):
                task.gens[m.group(1)] = SLOT.findall(m.group(2))
            elif m := INSTANCE.match(head):
                task.instances.append((m.group(1), m.group(2), BINDING.findall(m.group(3))))
            continue
        body = line.strip()
        if section == "relations" and (m := RELATION.match(body)):
            task.relations.append(m.groups())
        elif section == "priority":
            chain = [c.strip() for c in body.split("<")]
            task.priorities += list(zip(chain, chain[1:]))
        elif section == "cell" and (m := SET.match(body)):
            # Действует последняя запись; порядок — по первой.
            key, value = (m.group(1), m.group(2)), re.sub(r" {2,}", " ", m.group(3))
            task.given[key] = value
            if (vague := inexact(value)) is not None:
                task.inexact[key] = vague
            else:
                task.inexact.pop(key, None)
    return task


def precedes(task, a, b):
    edges = {}
    for hi, lo in task.priorities:
        edges.setdefault(hi, []).append(lo)
    seen, queue = set(), [a]
    while queue:
        for nxt in edges.get(queue.pop(0), []):
            if nxt == b:
                return True
            if nxt not in seen:
                seen.add(nxt)
                queue.append(nxt)
    return False


def derive(task):
    """Решения и открытые вопросы задачи, в порядке fp.Derive. Перенос —
    цепочкой: выведенное — посылка для следующего шага, правила решения —
    правила всего его пути (кольцо 1 перемножает их надёжность)."""

    def narrow(key, d):
        """Решение о неточной цели — внутри неё, альтернативы — её варианты."""
        vague = task.inexact.get(key)
        if vague is None:
            return d
        if not vague.admits(d.value):
            return None
        if vague.variants and all(NAME.match(v) for v in vague.variants):
            d.alternatives = vague.variants
        return d

    def resolve(key, sources):
        """Решение цели по её источникам — (откуда, значение, отношение, тип,
        правила пути) — или вопрос оператору."""
        target, attr = key
        values = list(dict.fromkeys(s[1] for s in sources))
        outside = lambda: Open([target], attr, values + [task.given[key]])
        if len(values) == 1:
            s = sources[0]
            d = narrow(key, Decision(target, attr, values[0], f"relation({s[2]})", s[4] + [f"fp.{s[3]}"]))
            return (d, None) if d is not None else (None, outside())
        win = next((s for s in sources if all(o[1] == s[1] or precedes(task, s[0], o[0]) for o in sources)), None)
        if win is None:
            return None, Open([target], attr, values)
        lose = next(o[0] for o in sources if o[1] != win[1])
        # Приоритет — посылка оператора, точная: правило шага одно — перенос.
        d = narrow(key, Decision(target, attr, win[1], f"priority({win[0]} < {lose})", win[4] + [f"fp.{win[3]}"],
                                 values if all(NAME.match(v) for v in values) else None))
        return (d, None) if d is not None else (None, outside())

    # Перенос по шагам: шаг — посылки и решения прошлого шага. blocked —
    # цели, которым разные пути дают разные значения: вопросы оператору, от
    # них ничего не переносится. С каждым кругом blocked растёт, поэтому
    # вывод кончается и на отношениях по кругу.
    blocked = {}
    while True:
        decided, decisions, opened, visited = {}, [], [], set()

        def value_of(key):
            if task.exact(key):
                return task.given[key], []
            d = decided.get(key)
            return (d.value, d.rules) if d else (None, None)

        frontier = [k for k in task.given if task.exact(k)]
        while frontier:
            carried, targets = {}, []
            for source, attr in frontier:
                value, rules = value_of((source, attr))
                for frm, kind, to, label in task.relations:
                    key = (to, attr)
                    if frm != source or kind not in CARRYING or not label or task.exact(key) or key in visited:
                        continue
                    if key not in carried:
                        carried[key] = []
                        targets.append(key)
                    carried[key].append((source, value, label, kind, rules))
            frontier = []
            for key in targets:
                visited.add(key)
                if key in blocked:
                    opened.append(Open([key[0]], key[1], blocked[key]))
                    continue
                d, q = resolve(key, carried[key])
                if q is not None:
                    opened.append(q)
                    continue
                decided[key] = d
                decisions.append(d)
                frontier.append(key)
        # Другие пути: со всеми источниками — посылками и решениями всех
        # шагов — цель решилась бы иначе или не решилась бы вовсе.
        again = False
        for d in decisions:
            key, every = (d.target, d.attr), []
            for frm, kind, to, label in task.relations:
                if to != d.target or kind not in CARRYING or not label:
                    continue
                value, rules = value_of((frm, d.attr))
                if value is not None:
                    every.append((frm, value, label, kind, rules))
            other, q = resolve(key, every)
            if q is not None:
                blocked[key], again = q.values, True
            elif other.value != d.value:
                blocked[key], again = list(dict.fromkeys(s[1] for s in every)), True
        if not again:
            break

    labels = {label for *_, label in task.relations if label}
    for (source, attr), value in task.given.items():
        if source not in task.gens or source not in labels or attr in task.gens[source] or not task.exact((source, attr)):
            continue
        for name, of, _ in task.instances:
            if of == source and not task.exact((name, attr)):
                d = narrow((name, attr), Decision(name, attr, value, f"relation({source})", ["fp.arrow"]))
                if d is not None:
                    decisions.append(d)

    by_attr = {}
    for d in decisions:
        by_attr.setdefault(d.attr, [])
        if d.value not in by_attr[d.attr]:
            by_attr[d.attr].append(d.value)
    kept, split = [], {}
    for d in decisions:
        if len(by_attr[d.attr]) == 1:
            kept.append(d)
            continue
        if d.attr not in split:
            split[d.attr] = Open([], d.attr, by_attr[d.attr])
            opened.append(split[d.attr])
        split[d.attr].targets.append(d.target)
    return kept, opened


def report_lines(decisions):
    lines, seen = [], set()
    for d in decisions:
        if d.attr in seen:
            continue
        seen.add(d.attr)
        lines += [f"  q: {d.attr}\n", f"    choice: {d.value}\n"]
        if d.alternatives:
            lines.append(f"    alternatives: {{{', '.join(d.alternatives)}}}\n")
        lines.append(f"    basis: {d.basis}\n")
    return lines


class AnswerPlan:
    """Ответ, который строит логика модели: копия задачи, ячейка с
    решениями, отчёт. Порядок решений посылки не задают — его выбирает
    сеть; остальное следует из написанного."""

    def __init__(self, text, decisions=None):
        self.task = parse(text)
        self.copy = canonical(text)
        derived, self.open = derive(self.task)
        self.decisions = derived if decisions is None else decisions
        taken = set(self.task.cells) | {n for n, _, _ in self.task.instances} | set(self.task.gens)
        taken |= {label for *_, label in self.task.relations if label}
        self.cell = next(f"c{k}" for k in range(1, 10000) if f"c{k}" not in taken)

    def next(self, output):
        """(допустимые утверждения после output, ведёт ли логика это место);
        пустой список — ответ закончен."""
        if not output.startswith(self.copy):
            return [], False
        rest = output[len(self.copy):]
        if not self.decisions:
            return [], rest == ""
        lines = rest.splitlines(keepends=True)
        if not lines:
            return ["\n"], True
        if len(lines) == 1:
            return [f"cell {self.cell}:\n"], True
        written, i = [], 2
        while i < len(lines) and lines[i].startswith("  set "):
            d = next((d for d in self.decisions if d.line() == lines[i]), None)
            if d is None:
                return [], False
            written.append(d)
            i += 1
        if i == len(lines):
            adm = [d.line() for d in self.decisions if d not in written]
            return (adm or ["\n"]), True
        report = ["report:\n"] + report_lines(written)
        done = len(lines) - i - 1
        if lines[i] != "\n" or done > len(report):
            return [], False
        return ([] if done == len(report) else [report[done]]), True

    def text(self):
        out = self.copy
        while True:
            adm, ok = self.next(out)
            if not ok or not adm:
                return out
            out += adm[0]


CLASS_LINE = re.compile(r"(\w+)\s*:\s*(.*)$")
WORD = re.compile(r'"([^"]*)"')


def value_kind(value, is_word=None):
    """Вид значения правки: неточное, число с единицей, слово языка или
    частное имя — как fp.Shape."""
    if (vague := inexact(value)) is not None:
        return "{…}" if vague.variants is not None else ".." + vague.unit
    # Число — как fp.ParseNumber: цифры и точки, дальше единица.
    head, unit = re.match(r"([0-9.]*)(.*)$", value.strip(), re.S).groups()
    try:
        float(head)
        return "#" + unit.strip()
    except ValueError:
        pass
    if is_word is not None and is_word(value):
        return value
    return "_"


def shape(text, is_word=None):
    """Структура задачи без частных имён — те же факты, что fp.Shape у
    ochan: цель, классы, отношения, приоритеты, жёсткие правила, обобщения,
    частные случаи по обобщению и правки «Цель.атрибут» с видом значения.
    Две записи одной структуры на разных хостах дают одни и те же факты:
    по ним модель узнаёт кластер повторяющихся инцидентов (learn.py)."""
    facts, class_of, goal, sets, particular, section = set(), {}, None, [], set(), None
    for line in text.replace("\r", "").split("\n"):
        line = COMMENT.sub(r"\1", line).rstrip()
        if not line.strip():
            continue
        if not line[0].isspace():
            head = line.strip()
            section = head.split(":")[0].split()[0]
            if section == "goal":
                goal = head.split(":", 1)[1].strip()
            elif m := CELL.match(head):
                section = "cell"
            elif GENERALIZATION.match(head):
                section = "generalization"
            elif m := INSTANCE.match(head):
                particular.add(m.group(1))
                facts.add("instance of " + m.group(2))
            elif m := RULE.match(head):
                kind = "forbid" if head.startswith(("hard forbid", "??n_")) else "require"
                facts.add(f"rule {kind} {m.group(1)}")
            continue
        body = line.strip()
        if section == "classes" and (m := CLASS_LINE.match(body)):
            facts.add("class " + m.group(1))
            for word in WORD.findall(m.group(2)):
                class_of[word] = m.group(1)
        elif section == "relations" and (m := RELATION.match(body)):
            facts.add(f"relation {m.group(1)} {m.group(2)} {m.group(3)}")
        elif section == "priority":
            chain = [c.strip() for c in body.split("<")]
            facts.update(f"priority {hi} < {lo}" for hi, lo in zip(chain, chain[1:]))
        elif section == "generalization" and (m := OPERATION.search(body)):
            facts.add(f"generalization {body.split('=')[0].strip()} = {m.group(1)}")
        elif section == "cell" and (m := SET.match(body)):
            sets.append(m.groups())
    if goal is not None:
        facts.add("goal " + class_of.get(goal, ""))
    for root, attr, value in sets:
        facts.add(f"set {'_' if root in particular else root}.{attr} {value_kind(re.sub(r' {2,}', ' ', value), is_word)}")
    return sorted(facts)


def similarity(a, b):
    """Похожесть двух структур: 1 − δ, δ — расстояние Жаккара по фактам
    (А.5 манифеста), как fp.Similarity. Пустые — одинаковы."""
    a, b = set(a), set(b)
    return len(a & b) / len(a | b) if a | b else 1.0


def missing(text):
    """Недостающие предпосылки (⊥undef): требование, действие правила или
    операция обобщения вне K, класс скилла не из classes:, обобщение
    стрелки или незаполненные её слоты."""
    task = parse(text)
    unbound = [f"{name}: {of}" if of not in task.gens else
               f"{name}: {', '.join(s for s in task.gens[of] if s not in bound)}"
               for name, of, bound in task.instances if of not in task.gens or set(task.gens[of]) - set(bound)]
    classes, skill_classes, known, needed, section = set(), [], set(BUILTINS), [], None
    for line in text.replace("\r", "").split("\n"):
        line = COMMENT.sub(r"\1", line).rstrip()
        if not line.strip():
            continue
        if not line[0].isspace():
            head = line.strip()
            section = head.split(":")[0].split()[0]
            if section == "requires":
                needed += [n.strip() for n in head.split(":", 1)[1].split(",") if n.strip()]
            for pattern in (GENERALIZATION, INSTANCE):
                if m := pattern.match(head):
                    known.add(m.group(1))
            if m := RULE.match(head):
                needed.append(m.group(1))
            continue
        body = line.strip()
        if section == "classes":
            classes.add(body.split(":")[0].strip())
        elif section == "relations":
            m = RELATION.match(body)
            if m and m.group(4):
                known.add(m.group(4))
        elif section == "import":
            key, _, items = body.partition(":")
            names = [n.strip() for n in items.split(",") if n.strip()]
            if key.strip() == "provides":
                skill_classes += [n for n in names if n[0].isupper()]
                known.update(n for n in names if not n[0].isupper())
            elif key.strip() == "requires_class":
                skill_classes += names
        elif section == "generalization":
            m = OPERATION.search(body)
            if m:
                needed.append(m.group(1))
    return [n for n in needed if n not in known] + [c for c in skill_classes if c not in classes] + unbound
