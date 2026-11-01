"""Пакеты знаний своей модели — через реестр ochan, с подписью.

Знания — то, чему модель научилась на одной машине и что годится другой:
обобщения задач, которые повторялись (learn.py), и как на их структуре
работали правила вывода. Частных случаев в пакете нет: обобщение — факты
logic.shape без частных имён, вместо номеров инцидентов — их число. Хосты,
числа и журналы машины, где модель училась, с пакетом не уходят.

Пакет знаний — обычный пакет реестра: package.yaml с версией и
knowledge.yaml (fp.ParseKnowledge проверяет его при публикации и установке).
Дорога — та же, что у скиллов: реестр подписан ed25519, архив сверяется с
суммой из подписанного индекса, ставит его заявка.

    python learn.py --model M --package pg_incidents --version 1.0
    ochan '!!registry' publish pg_incidents --registry REG --key registry.key
    ochan '!!rin' @pg_incidents 1.0          # на другой машине: подпись, сумма, заявка
    python adapter.py serve --model M --modules ~/.octochan/modules

Модель берёт знания пакета только для задачи, которая его подключает —
import @pg_incidents v1, тот же import, что у скилла, — и повторяет его
обобщение. Версия — часть знания: задача просит v1, а в каталоге 2.0 —
знаний этой версии у модели нет, и это недостающая предпосылка.
"""

import os
import re
import time

import yaml

import logic

FILE = "knowledge.yaml"
MANIFEST = "package.yaml"
PROTOCOL = 1
MAX_FILE = 64 << 20

KEY = re.compile(r"[A-Za-z0-9_]{1,64}")
RULE = re.compile(r"fp\.[a-z_]+")
PACKAGE = re.compile(r"[^\W\d]\w*")
VERSION = re.compile(r"v?(\d+(?:\.\d+){0,2})")
# Как importPattern у ochan: import @скилл [vN.N].
IMPORT = re.compile(r"import\s+@([^\W][\w.\-]*)(?:\s+(v\d+(?:\.\d+)*))?")
FIELDS = {"key", "shape", "similarity", "instances", "outcomes", "rules"}


def matches(want, have):
    """Версия have подходит под требование want — как fp.VersionMatches:
    v2 — под 2.0 и 2.7.1, v2.3 — под 2.3.x."""
    want, have = want.removeprefix("v"), have.removeprefix("v")
    if not want:
        return True
    if not have:
        return False
    w, h = want.split("."), have.split(".")
    h += ["0"] * (len(w) - len(h))
    return all(a.lstrip("0") == b.lstrip("0") for a, b in zip(w, h))


def imports(task):
    """Пакеты, которые задача подключает: (имя, версия или "")."""
    out = []
    for line in task.replace("\r", "").split("\n"):
        line = logic.COMMENT.sub(r"\1", line).strip()
        if m := IMPORT.fullmatch(line):
            out.append((m.group(1), m.group(2) or ""))
    return out


def counts(value, where):
    if not (isinstance(value, list) and len(value) == 2 and all(isinstance(x, (int, float)) and not isinstance(x, bool)
                                                                 for x in value)):
        raise ValueError(f"{where}: ожидалось [успехов, исходов]")
    if not (0 <= value[0] <= value[1]) or any(x != x or abs(x) == float("inf") for x in value):
        raise ValueError(f"{where}: [{value[0]}, {value[1]}] — успехов от 0 до числа исходов")
    return [float(value[0]), float(value[1])]


def parse(data, source):
    """Знания пакета, проверенные так же, как fp.ParseKnowledge."""
    k = yaml.safe_load(data)
    if not isinstance(k, dict):
        raise ValueError(f"{source}: ожидалось protocol и generalizations")
    if extra := set(k) - {"protocol", "generalizations"}:
        raise ValueError(f"{source}: неизвестное поле {sorted(extra)[0]}")
    if (k.get("protocol") or 1) > PROTOCOL:
        raise ValueError(f"{source}: знания протокола v{k['protocol']}, модель понимает v{PROTOCOL}")
    gens = k.get("generalizations") or []
    if not isinstance(gens, list) or not gens:
        raise ValueError(f"{source}: нет обобщений")
    out, seen = [], set()
    for i, g in enumerate(gens, 1):
        where = f"{source}: обобщение {i}"
        if not isinstance(g, dict):
            raise ValueError(f"{where} пусто")
        if extra := set(g) - FIELDS:
            raise ValueError(f"{where}: неизвестное поле {sorted(extra)[0]}")
        key, shape, similarity = g.get("key"), g.get("shape"), g.get("similarity")
        if not (isinstance(key, str) and KEY.fullmatch(key)) or key in seen:
            raise ValueError(f"{where}: ключ {key!r} — буквы, цифры и _, без повторов")
        if not (isinstance(shape, list) and shape and all(isinstance(f, str) and f.strip() and "\n" not in f for f in shape)):
            raise ValueError(f"{where} ({key}): структура — непустой список фактов")
        if not (isinstance(similarity, (int, float)) and 0 < similarity <= 1):
            raise ValueError(f"{where} ({key}): similarity — доля, больше 0 и не больше 1")
        instances = g.get("instances", 0)
        if not isinstance(instances, int) or instances < 0:
            raise ValueError(f"{where} ({key}): instances — не меньше 0")
        rules = g.get("rules") or {}
        if not isinstance(rules, dict) or not all(isinstance(r, str) and RULE.fullmatch(r) for r in rules):
            raise ValueError(f"{where} ({key}): правила — fp.<тип>")
        seen.add(key)
        out.append({"key": key, "shape": sorted(shape), "similarity": float(similarity), "instances": instances,
                    "outcomes": counts(g.get("outcomes"), f"{where} ({key}): outcomes"),
                    "rules": {r: counts(c, f"{where} ({key}): {r}") for r, c in rules.items()}})
    return out


def version_of(directory):
    """Версия пакета из package.yaml (строки «ключ: значение»)."""
    path = os.path.join(directory, MANIFEST)
    if not os.path.isfile(path):
        return ""
    with open(path, encoding="utf-8-sig") as f:
        for line in f:
            key, sep, value = line.partition(":")
            if sep and key.strip() == "version" and (m := VERSION.fullmatch(value.strip())):
                return m.group(1)
    return ""


class Library:
    """Пакеты знаний в каталоге модулей ochan. Каталог перечитывается не
    чаще раза в секунду и только когда в нём что-то поменялось: пакет
    поставили, обновили или убрали, а модель работает дальше."""

    def __init__(self, modules):
        self.modules = modules
        self.packages, self.errors = {}, {}
        self.generation = 0
        self._stamp, self._checked = None, 0.0

    def refresh(self):
        if not self.modules or time.monotonic() - self._checked < 1.0:
            return
        self._checked = time.monotonic()
        stamp = []
        if os.path.isdir(self.modules):
            for name in sorted(os.listdir(self.modules)):
                file = os.path.join(self.modules, name, FILE)
                if os.path.isfile(file):
                    manifest = os.path.join(self.modules, name, MANIFEST)
                    stamp.append((name, os.path.getmtime(file), os.path.getsize(file),
                                  os.path.getmtime(manifest) if os.path.isfile(manifest) else 0))
        if stamp == self._stamp:
            return
        packages, errors = {}, {}
        for name, *_ in stamp:
            directory, file = os.path.join(self.modules, name), os.path.join(self.modules, name, FILE)
            try:
                if os.path.getsize(file) > MAX_FILE:
                    raise ValueError(f"{file} больше {MAX_FILE >> 20} МиБ")
                with open(file, encoding="utf-8") as f:
                    gens = parse(f.read(), file)
                version = version_of(directory)
                if not version:
                    raise ValueError(f"{directory}: у пакета знаний нет версии ({MANIFEST})")
                packages[name] = (version, gens)
            except (OSError, ValueError, yaml.YAMLError) as e:
                errors[name] = str(e)
        self.packages, self.errors, self._stamp = packages, errors, stamp
        self.generation += 1

    def listed(self):
        """Пакеты знаний, которые видит модель: «имя версия»."""
        self.refresh()
        return [f"{name} {version}" for name, (version, _) in sorted(self.packages.items())]

    def for_task(self, task):
        """(обобщения пакетов, которые задача подключает, — как кластеры;
        недостающее). Пакет без знаний — скилл: он не модели."""
        self.refresh()
        clusters, missing = {}, []
        for name, want in imports(task):
            if name in self.errors:
                missing.append(f"@{name}: знания пакета негодны — {self.errors[name]}")
                continue
            if name not in self.packages:
                continue
            version, gens = self.packages[name]
            if not matches(want, version):
                missing.append(f"@{name} {want}: знаний этой версии у модели нет — в каталоге {version}")
                continue
            for g in gens:
                clusters[f"@{name}/{g['key']}"] = {k: g[k] for k in ("shape", "similarity", "instances", "outcomes", "rules")}
        return clusters, missing


def export(clusters, directory, version, description=""):
    """Пакет знаний из кластеров модели: package.yaml, knowledge.yaml и
    README.md в directory; имя пакета — имя каталога. Номера инцидентов не
    выгружаются — только их число."""
    name = os.path.basename(os.path.normpath(directory))
    if not PACKAGE.fullmatch(name):
        raise ValueError(f"пакет {name!r}: имя — имя каталога, буквы, цифры и _, первой — буква")
    if not VERSION.fullmatch(version):
        raise ValueError(f"версия {version!r}: вида 1, 1.2 или 1.2.3")
    version = VERSION.fullmatch(version).group(1)
    if "\n" in description or ":" in description:
        raise ValueError("описание — одна строка без «:»")
    if not clusters:
        raise ValueError("кластеров в параметрах модели нет — выгружать нечего (learn.py --clusters)")
    gens = [{"key": key, "shape": c["shape"], "similarity": c["similarity"], "instances": len(c["instances"]),
             "outcomes": c["outcomes"], "rules": c["rules"]} for key, c in sorted(clusters.items())]
    text = ("# Знания своей модели ℒ: обобщения задач, которые повторялись, и надёжность\n"
            "# правил вывода на их структуре (llama/learn.py --package). Частных случаев нет.\n"
            + yaml.safe_dump({"protocol": PROTOCOL, "generalizations": gens}, allow_unicode=True, sort_keys=False))
    parse(text, FILE)
    os.makedirs(directory, exist_ok=True)
    with open(os.path.join(directory, FILE), "w", encoding="utf-8") as f:
        f.write(text)
    with open(os.path.join(directory, MANIFEST), "w", encoding="utf-8") as f:
        f.write(f"version: {version}\n" + (f"description: {description}\n" if description else ""))
    lines = [f"# @{name} v{version}", "",
             "Знания своей модели ℒ — пакет реестра ochan. Модель берёт их для задачи с",
             f"`import @{name} v{version.split('.')[0]}`, которая повторяет обобщение.", ""]
    for g in gens:
        solved, total = g["outcomes"]
        lines += [f"## {g['key']}", "", f"Частных случаев {g['instances']}, исходов {total:.1f}, модель решила сама {solved:.1f}.",
                  ""] + [f"- `{fact}`" for fact in g["shape"]] + [""]
        lines += [f"Правило `{rule}`: верно {k:.1f} из {n:.1f}." for rule, (k, n) in sorted(g["rules"].items())] + [""]
    with open(os.path.join(directory, "README.md"), "w", encoding="utf-8") as f:
        f.write("\n".join(lines))
    return name, len(gens)
