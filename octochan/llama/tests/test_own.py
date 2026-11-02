"""Своя модель ℒ: ответ пишет её логика, отказ без предпосылки — её ответ."""

import shutil
import unittest

from util import prompt, task, tiny_model

import own


class OwnModel(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir = tiny_model()
        cls.engine = own.Engine(cls.dir, "cpu")

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.dir, ignore_errors=True)

    def test_answer_is_logic(self):
        text = task("pg_memory.fp")
        ans = self.engine.generate(prompt(text))
        self.assertFalse(ans["refused"])
        plan, _, packages = self.engine.plan(text)
        self.assertTrue(ans["answer"].startswith(plan.copy))
        # Каждое выведенное логикой — в ответе, и ничего сверх допустимого.
        for p in packages:
            if p["verdict"] == "well_true":
                self.assertIn(p["statement"], ans["answer"])
        self.assertGreater(ans["forced"] + ans["chosen"], 0)

    def test_refuses_without_premise(self):
        text = task("pg_memory.fp") + "\nrequires: store\n"
        ans = self.engine.generate(prompt(text))
        self.assertTrue(ans["refused"])
        self.assertTrue(any(m.startswith("store") for m in ans["missing"]), ans["missing"])

    def test_deterministic(self):
        text = task("pg_memory.fp")
        self.assertEqual(self.engine.generate(prompt(text))["answer"], self.engine.generate(prompt(text))["answer"])


if __name__ == "__main__":
    unittest.main()
