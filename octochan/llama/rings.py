"""Кольца архитектурной модели ℒ — часть её кода (раздел 6.2 манифеста,
приложение А.7). Та же математика, что в пакете rings ochan: там она
судит ответ внешней модели снаружи, здесь — то, как своя модель решает.

- кольцо 1: поддержка пакета — надёжность правил по журналу (95-процентный
  интервал бета-распределения с условным счётом автора), умноженная на
  поддержку посылок; посылки задачи точные — единица;
- кольцо 2: пакет принят, если нижняя граница поддержки не ниже well_true;
  не принятый не пишется и уходит оператору вопросом;
- правило выбора между допустимыми пакетами: z = β₀ + β₁·log p₀ + β₂·L +
  β₃·(U − L) + β₄·G, P = σ(z); веса и поправку «оценка → наблюдаемая доля»
  даёт калибровка по журналу (fit.py), до неё — веса автора;
- калибровка: монотонная поправка по журналу (изотоническая регрессия);
- кластеры повторяющихся инцидентов (learn.py): задача, структура которой
  повторяет обобщение кластера, получает надёжность правил по исходам его
  инцидентов — на этой структуре, а не в среднем по журналу.

Числа — параметры модели, файл rings.json рядом с весами: их пересчитывают
fit.py из журнала решений ochan и learn.py из созревших кластеров, как
обучение пересчитывает веса. Каждая запись — новая версия в rings.versions/:
прежние остаются, к любой можно откатиться (rollback).
"""

import json
import math
import os
import shutil
from dataclasses import dataclass, field
from datetime import datetime, timezone

import logic

FILE = "rings.json"
VERSIONS = "rings.versions"
LOG = "log.json"


def beta_cdf(x, a, b):
    """Регуляризованная неполная бета-функция I_x(a, b)."""
    if x <= 0:
        return 0.0
    if x >= 1:
        return 1.0
    front = math.exp(a * math.log(x) + b * math.log(1 - x) + math.lgamma(a + b) - math.lgamma(a) - math.lgamma(b))
    if x < (a + 1) / (a + b + 2):
        return front * _fraction(x, a, b) / a
    return 1 - front * _fraction(1 - x, b, a) / b


def _fraction(x, a, b):
    tiny = 1e-300
    qab, qap, qam = a + b, a + 1, a - 1
    c, d = 1.0, 1 - qab * x / qap
    d = 1 / (d if abs(d) > tiny else tiny)
    h = d
    for m in range(1, 501):
        m2 = 2 * m
        for num in (m * (b - m) * x / ((qam + m2) * (a + m2)), -(a + m) * (qab + m) * x / ((a + m2) * (qap + m2))):
            d = 1 + num * d
            d = 1 / (d if abs(d) > tiny else tiny)
            c = 1 + num / c
            c = c if abs(c) > tiny else tiny
            step = d * c
            h *= step
        if abs(step - 1) < 1e-15:
            break
    return h


def beta_quantile(p, a, b):
    lo, hi = 0.0, 1.0
    for _ in range(200):
        mid = (lo + hi) / 2
        lo, hi = (mid, hi) if beta_cdf(mid, a, b) < p else (lo, mid)
    return (lo + hi) / 2


def reliability(successes, uses, prior=(8, 8)):
    """Надёжность правила: [L, U] Beta(k + a, n − k + b), a, b — с условным
    счётом автора prior = (успехов, применений). 8 из 8 без журнала —
    [0,66; 1,00]."""
    a = 1 + successes + prior[0]
    b = 1 + (uses - successes) + (prior[1] - prior[0])
    return beta_quantile(0.025, a, b), beta_quantile(0.975, a, b)


def share(k, n):
    """Доля прочтения среди n: 95-процентный интервал Beta(k + 1, n − k + 1)."""
    return beta_quantile(0.025, k + 1, n - k + 1), beta_quantile(0.975, k + 1, n - k + 1)


def support(intervals):
    """Поддержка вывода по правилам с точными посылками: произведение."""
    lo, hi = 1.0, 1.0
    for l, u in intervals:
        lo, hi = lo * l, hi * u
    return lo, hi


@dataclass
class Params:
    """Параметры колец модели: из rings.json, иначе по умолчанию."""
    well_true: float = 0.5
    prior: tuple = (8, 8)
    # правило -> [успехов, применений] по журналу, с весом давности.
    rules: dict = field(default_factory=dict)
    # β₀..β₄ правила выбора.
    weights: list = field(default_factory=lambda: [0.0, 1.0, 1.0, -1.0, 1.0])
    # Калибровка: точки (оценка, наблюдаемая доля), монотонные.
    calibration: list = field(default_factory=list)
    # История прочтений (раздел 5.2): атрибут -> {значение: сколько раз
    # оператор задал его точно или одобрил такое сужение}.
    history: dict = field(default_factory=dict)
    # Кластеры повторяющихся инцидентов (learn.py): ключ -> {"shape":
    # обобщение — структура без частных имён, "similarity": порог похожести,
    # "instances": инциденты — его частные случаи, "outcomes": [решила модель
    # сама, исходов всего], "rules": правило -> [успехов, применений] на этой
    # структуре}.
    clusters: dict = field(default_factory=dict)

    @classmethod
    def load(cls, path):
        file = os.path.join(path, FILE)
        if not os.path.isfile(file):
            return cls()
        with open(file, encoding="utf-8") as f:
            return cls.of(json.load(f))

    @classmethod
    def of(cls, data):
        p = cls()
        for key, value in data.items():
            setattr(p, key, tuple(value) if key == "prior" else value)
        return p

    def save(self, path, note):
        """Пишет rings.json и его версию в rings.versions/ с заметкой, откуда
        она: прежние версии остаются. rings.json без версий — версия 0."""
        folder = os.path.join(path, VERSIONS)
        log = versions(path)
        os.makedirs(folder, exist_ok=True)
        if not log and os.path.isfile(os.path.join(path, FILE)):
            shutil.copyfile(os.path.join(path, FILE), os.path.join(folder, "0.json"))
            log.append({"version": 0, "time": None, "note": "до версий"})
        n = log[-1]["version"] + 1 if log else 1
        text = json.dumps(self.__dict__, ensure_ascii=False, indent=1)
        for file in (os.path.join(folder, f"{n}.json"), os.path.join(path, FILE)):
            with open(file, "w", encoding="utf-8") as f:
                f.write(text)
        log.append({"version": n, "time": datetime.now(timezone.utc).isoformat(timespec="seconds"), "note": note})
        with open(os.path.join(folder, LOG), "w", encoding="utf-8") as f:
            json.dump(log, f, ensure_ascii=False, indent=1)
        return n

    def rule(self, name, cluster=None):
        """Надёжность правила: на структуре кластера — по исходам его
        инцидентов, если правило там применялось, иначе по журналу."""
        successes, uses = (cluster or {}).get("rules", {}).get(name) or self.rules.get(name, (0, 0))
        return reliability(successes, uses, self.prior)

    def cluster_of(self, shape):
        """Кластер, обобщение которого повторяет структура задачи: самый
        похожий из тех, на которые она похожа не меньше их порога; иначе
        (None, None)."""
        best, found = -1.0, (None, None)
        for key, c in sorted(self.clusters.items()):
            s = logic.similarity(shape, c["shape"])
            if s >= c["similarity"] and s > best:
                best, found = s, (key, c)
        return found

    def outcomes(self, cluster):
        """Надёжность модели на структуре кластера: сколько его инцидентов
        она решила сама — тот же интервал, что у правила."""
        return reliability(*cluster["outcomes"], self.prior)

    def score(self, logprob, lower, upper, binding):
        """Итоговая оценка пакета: σ(z), затем поправка калибровки."""
        b0, b1, b2, b3, b4 = self.weights
        z = b0 + b1 * logprob + b2 * lower + b3 * (upper - lower) + b4 * binding
        return calibrated(1 / (1 + math.exp(-max(-60.0, min(60.0, z)))), self.calibration)


def versions(path):
    """Версии параметров модели: [{"version", "time", "note"}], от старых."""
    file = os.path.join(path, VERSIONS, LOG)
    if not os.path.isfile(file):
        return []
    with open(file, encoding="utf-8") as f:
        return json.load(f)


def rollback(path, version=None):
    """Откат к версии (по умолчанию — предыдущей): она записывается новой
    версией, так что откат тоже откатывается. Возвращает (к какой, новая)."""
    log = versions(path)
    if version is None:
        if len(log) < 2:
            raise ValueError("откатываться не к чему: версия одна")
        version = log[-2]["version"]
    file = os.path.join(path, VERSIONS, f"{version}.json")
    if not os.path.isfile(file):
        raise ValueError(f"версии {version} нет: {', '.join(str(v['version']) for v in log) or 'версий нет'}")
    with open(file, encoding="utf-8") as f:
        params = Params.of(json.load(f))
    return version, params.save(path, f"откат к версии {version}")


def calibrated(score, points):
    """Поправка «оценка → наблюдаемая доля»: кусочно-линейно по точкам."""
    if not points:
        return score
    points = sorted(points)
    if score <= points[0][0]:
        return points[0][1]
    for (x0, y0), (x1, y1) in zip(points, points[1:]):
        if score <= x1:
            return y0 + (y1 - y0) * (score - x0) / (x1 - x0) if x1 > x0 else y1
    return points[-1][1]


def isotonic(scores, outcomes, bins=10):
    """Калибровка по журналу: доля верных по интервалам оценок, сглаженная
    до монотонной (объединение соседних нарушителей). Точки — середины
    интервалов с наблюдаемой долей."""
    groups = [[0, 0, 0.0] for _ in range(bins)]  # верных, всего, сумма оценок
    for s, ok in zip(scores, outcomes):
        k = min(max(int(s * bins), 0), bins - 1)
        groups[k][0] += ok
        groups[k][1] += 1
        groups[k][2] += s
    blocks = [[g[0], g[1], g[2]] for g in groups if g[1]]
    merged = []
    for b in blocks:
        merged.append(b)
        while len(merged) > 1 and merged[-2][0] / merged[-2][1] > merged[-1][0] / merged[-1][1]:
            last = merged.pop()
            merged[-1] = [merged[-1][i] + last[i] for i in range(3)]
    return [(b[2] / b[1], b[0] / b[1]) for b in merged]


def fit_weights(features, outcomes, steps=2000, lr=0.1, l2=1e-3):
    """Веса правила выбора по журналу: логистическая регрессия на
    признаках (log p₀, L, U − L, G). Мало исходов — веса автора."""
    if len(features) < 20 or len(set(outcomes)) < 2:
        return None
    w = [0.0, 1.0, 1.0, -1.0, 1.0]
    for _ in range(steps):
        grad = [0.0] * 5
        for x, y in zip(features, outcomes):
            z = w[0] + sum(wi * xi for wi, xi in zip(w[1:], x))
            p = 1 / (1 + math.exp(-max(-60.0, min(60.0, z))))
            for i, xi in enumerate([1.0] + list(x)):
                grad[i] += (p - y) * xi
        w = [wi - lr * (g / len(features) + l2 * wi) for wi, g in zip(w, grad)]
    return w
