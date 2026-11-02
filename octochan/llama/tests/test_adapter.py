"""Адаптер: протокол движка v1 по HTTP вокруг своей модели."""

import json
import shutil
import threading
import unittest
import urllib.error
import urllib.request

from util import prompt, task, tiny_model

import adapter


class Adapter(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir = tiny_model()
        cls.engine = adapter.engine_for(cls.dir, "cpu")
        cls.server = adapter.make_server(cls.engine, "127.0.0.1:0")
        cls.url = "http://127.0.0.1:%d" % cls.server.server_address[1]
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        shutil.rmtree(cls.dir, ignore_errors=True)

    def call(self, path, body=None, raw=None):
        data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
        req = urllib.request.Request(self.url + path, data=data, method="POST" if data is not None else "GET")
        req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                return resp.status, json.loads(resp.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read() or b"{}")

    def test_info(self):
        code, info = self.call("/v1/info")
        self.assertEqual(code, 200)
        self.assertEqual(info["protocol"], adapter.PROTOCOL)
        self.assertTrue(info["generate"])
        self.assertEqual(info["model"], self.dir)
        self.assertEqual(info["knowledge"], [])

    def test_generate_matches_engine(self):
        text = task("pg_memory.fp")
        code, ans = self.call("/v1/generate", {"prompt": prompt(text)})
        self.assertEqual(code, 200)
        self.assertEqual(ans["answer"], self.engine.generate(prompt(text))["answer"])

    def test_next_is_distribution(self):
        code, step = self.call("/v1/next", {"prompt": prompt(task("pg_memory.fp")), "output": "", "top": 2})
        self.assertEqual(code, 200)
        total = step["end"] + sum(c["p"] for c in step["candidates"])
        self.assertLessEqual(total, 1 + 1e-6)
        # Начало ответа — копия задачи: допустимо одно продолжение.
        self.assertEqual(len(step["candidates"]), 1)
        self.assertAlmostEqual(step["candidates"][0]["p"], 1.0, places=5)

    def test_errors(self):
        code, _ = self.call("/v1/nothing", {"prompt": ""})
        self.assertEqual(code, 404)
        code, body = self.call("/v1/generate", raw=b"{not json")
        self.assertEqual(code, 500)
        self.assertIn("error", body)

    def test_local_only(self):
        with self.assertRaises(SystemExit):
            adapter.check_listen("0.0.0.0:8093", allow_remote=False)
        adapter.check_listen("127.0.0.1:8093", allow_remote=False)


if __name__ == "__main__":
    unittest.main()
