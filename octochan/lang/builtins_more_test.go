package lang

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFormat(t *testing.T) {
	cases := map[string]string{
		`print(format("id=%d", 42))`:                         "id=42",
		`print(format("%s: %.2f%%", "cpu", 93.456))`:         "cpu: 93.46%",
		`print(format("%5d|%-5s|", 7, "ab"))`:                "    7|ab   |",
		`print(format("%v %v", [1, 2], {"a": 1}))`:           `[1, 2] {"a": 1}`,
		`print(format("%q", "x y"))`:                         `"x y"`,
		`print(format("%x %t", 255, true))`:                  "ff true",
		`print(format("%d", 3.0))`:                           "3",
		`x = "кириллица"` + "\n" + `print(format("%s!", x))`: "кириллица!",
	}
	for src, want := range cases {
		if got := strings.TrimRight(mustRun(t, src), "\n"); got != want {
			t.Errorf("%s: %q, ожидалось %q", src, got, want)
		}
	}
	for src, want := range map[string]string{
		`format("%d", "x")`:   "нужно целое",
		`format("%d %d", 1)`:  "не хватает значения",
		`format("%d", 1, 2)`:  "лишние значения",
		`format("%k", 1)`:     "неизвестный глагол",
		`format("100%")`:      "обрывается на %",
		`format("%f", "1.5")`: "нужно число",
	} {
		if _, err := run(t, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", src, err, want)
		}
	}
}

func TestMath(t *testing.T) {
	cases := map[string]string{
		`print(sqrt(16))`:                     "4",
		`print(pow(2, 10))`:                   "1024",
		`print(abs(-3), abs(-2.5))`:           "3 2.5",
		`print(round(2.5))`:                   "3",
		`print(round(3.14159, 2))`:            "3.14",
		`print(floor(2.9), ceil(2.1))`:        "2 3",
		`print(min(3, 1, 2), max([4, 9, 2]))`: "1 9",
		`print(min("b", "a"))`:                "a",
		`print(float("2.5") + 1)`:             "3.5",
	}
	for src, want := range cases {
		if got := strings.TrimSpace(mustRun(t, src)); got != want {
			t.Errorf("%s: %q, ожидалось %q", src, got, want)
		}
	}
	if _, err := run(t, `sqrt(-1)`); err == nil {
		t.Error("sqrt(-1) прошёл")
	}
	if _, err := run(t, `min([])`); err == nil {
		t.Error("min([]) прошёл")
	}
}

func runWith(t *testing.T, src string, setup func(*Interpreter)) (string, error) {
	t.Helper()
	file, err := Parse("<тест>", src)
	if err != nil {
		return "", err
	}
	registry := NewRegistry()
	for _, decl := range file.Aliases {
		if err := registry.Add("", decl); err != nil {
			return "", err
		}
	}
	var out strings.Builder
	interp := NewInterpreter(context.Background(), registry, &out)
	setup(interp)
	err = interp.RunFile(file)
	return out.String(), err
}

// С одним зерном rand даёт одно и то же — и в последовательном коде, и в
// итерациях parallel for, которые идут в любом порядке.
func TestRandIsDeterministicWithSeed(t *testing.T) {
	src := `print(rand(1000), rand(5, 10), rand() < 1, choice(["a", "b", "c"]))
print(shuffle([1, 2, 3, 4, 5]))
parallel for i in [1, 2, 3, 4] {
    print(i, rand(1000000))
}`
	seeded := func(in *Interpreter) { in.SetSeed(42) }
	first, err := runWith(t, src, seeded)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, _ := runWith(t, src, seeded)
		if again != first {
			t.Fatalf("с тем же зерном другой результат:\n%s\n---\n%s", first, again)
		}
	}
	other, _ := runWith(t, src, func(in *Interpreter) { in.SetSeed(7) })
	if other == first {
		t.Fatal("с другим зерном тот же результат")
	}
}

func TestTimeIsTheRunClock(t *testing.T) {
	clock := time.Date(2026, 10, 23, 12, 30, 5, 0, time.FixedZone("MSK", 3*3600))
	out, err := runWith(t, `print(now())
print(time.unix())
print(time.format("%Y-%m-%d %H:%M:%S %z"))
print(time.add(now(), "1h30m"))
print(time.format("%F", time.add(now(), 86400)))
print(sleep("1ms"), time.sleep(0))
print(now() == time.now())`, func(in *Interpreter) { in.SetClock(clock) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"2026-10-23T12:30:05+03:00",
		fmt.Sprint(clock.Unix()),
		"2026-10-23 12:30:05 +0300",
		"2026-10-23T14:00:05+03:00",
		"2026-10-24",
		"1ms 0s",
		"true",
	}
	if got := strings.Split(strings.TrimSpace(out), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("время:\n%s\nожидалось:\n%s", out, strings.Join(want, "\n"))
	}
	if _, err := run(t, `sleep("2h")`); err == nil {
		t.Error("пауза в два часа прошла")
	}
	if _, err := run(t, `time.format("%Q")`); err == nil {
		t.Error("неизвестный знак образца прошёл")
	}
}

// В режиме плана sleep не ждёт.
func TestSleepIsSkippedInPlan(t *testing.T) {
	file, _ := Parse("<тест>", `sleep(30)`)
	interp := NewInterpreter(context.Background(), NewRegistry(), &strings.Builder{})
	interp.SetPlanMode()
	started := time.Now()
	if err := interp.RunFile(file); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("план ждал паузу")
	}
}

func TestSerialization(t *testing.T) {
	cases := map[string]string{
		`print(to_json({"b": [1, 2.5, true, null], "a": "x"}))`:                                                     `{"a":"x","b":[1,2.5,true,null]}`,
		`d = from_json("{\"n\": 3, \"f\": 1.5, \"l\": [1, 2]}")` + "\nprint(d[\"n\"] + 1, d[\"f\"], len(d[\"l\"]))": "4 1.5 2",
		`print(to_yaml({"name": "db1", "port": 5432}))`:                                                             "name: db1\nport: 5432",
		`print(from_yaml("a: 1\nb: [x, y]\n")["b"][1])`:                                                             "y",
		`print(to_toml({"db": {"port": 5432}, "name": "x"}))`:                                                       "name = 'x'\n\n[db]\nport = 5432",
		`print(from_toml("[db]\nport = 5432\n")["db"]["port"] + 1)`:                                                 "5433",
		`print(to_json(from_json("[1,{\"k\":\"v\"}]")))`:                                                            `[1,{"k":"v"}]`,
	}
	for src, want := range cases {
		if got := strings.TrimSpace(mustRun(t, src)); got != want {
			t.Errorf("%s:\n%q\nожидалось\n%q", src, got, want)
		}
	}
	for src, want := range map[string]string{
		`from_json("{")`:    "JSON не разбирается",
		`from_json("1 2")`:  "лишний текст",
		`to_toml([1, 2])`:   "таблица",
		`to_json(stdout())`: "не сериализуется",
		`from_yaml("a: [")`: "YAML не разбирается",
		`from_toml("a = ")`: "TOML не разбирается",
	} {
		if _, err := run(t, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", src, err, want)
		}
	}
}

func TestEval(t *testing.T) {
	out := mustRun(t, `alias len(x) { return 0 }
print(eval("2 + 3 * 4"), eval("format(\"%d\", 7)"), eval("len([1, 2])"), eval("call(fn(x) => x * 2, 21)"))`)
	// Алиас len встроенную функцию в eval не перекрывает.
	if strings.TrimSpace(out) != "14 7 2 42" {
		t.Fatalf("eval: %q", out)
	}
	for src, want := range map[string]string{
		`eval("x = 1")`:   "одно выражение",
		`eval("1 +")`:     "eval",
		`eval("nope(1)")`: "неизвестная функция",
		`eval(5)`:         "нужна строка",
		// Алиасы из строки заявка не увидела бы.
		"alias double(x) { return x * 2 }\neval(\"double(21)\")": "неизвестная функция",
		`eval("fn(x) => x")`: "а не функцию",
	} {
		if _, err := run(t, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", src, err, want)
		}
	}
}

func TestFetchAndCurl(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"version": "16.2", "ok": true}`)
		case "/auth":
			fmt.Fprint(w, r.Header.Get("Authorization"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	src := fmt.Sprintf(`v = from_json(fetch("%[1]s/json"))
print(v["version"])
r = curl("%[1]s/missing")
print(r["status"])
print(fetch("%[1]s/auth", {"headers": {"Authorization": "Bearer t"}}))
print(curl("%[1]s/json", {"method": "HEAD"})["status"])`, server.URL)
	out := mustRun(t, src)
	if got := strings.Fields(out); strings.Join(got, " ") != "16.2 404 Bearer t 200" {
		t.Fatalf("fetch/curl: %q", out)
	}

	for src, want := range map[string]string{
		fmt.Sprintf(`fetch("%s/missing")`, server.URL):                 "404",
		fmt.Sprintf(`curl("%s/json", {"method": "POST"})`, server.URL): "только читает",
		`fetch("file:///etc/passwd")`:                                  "http:// или https://",
		fmt.Sprintf(`fetch("%s/json", {"body": "x"})`, server.URL):     "неизвестный параметр",
	} {
		if _, err := run(t, src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, ожидалось %q", src, err, want)
		}
	}
}

// В песочнице сеть закрыта, пока хост не разрешён; обращение видно в плане.
func TestFetchInSandbox(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()

	box, err := NewSandbox([]string{t.TempDir()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`print(fetch("%s/x"))`, server.URL)
	_, err = runWith(t, src, func(in *Interpreter) { in.SetSandbox(box) })
	if err == nil || !strings.Contains(err.Error(), "сеть в песочнице закрыта") {
		t.Fatalf("сеть в песочнице: %v", err)
	}

	host := strings.TrimPrefix(server.URL, "http://")
	if err := box.AllowNet([]string{host}); err != nil {
		t.Fatal(err)
	}
	file, _ := Parse("<тест>", src)
	interp := NewInterpreter(context.Background(), NewRegistry(), &strings.Builder{})
	interp.SetSandbox(box)
	interp.SetPlanMode()
	if err := interp.RunFile(file); err != nil {
		t.Fatal(err)
	}
	effects := interp.Effects()
	if len(effects) != 1 || effects[0].Kind != EffectLive || !strings.HasPrefix(effects[0].Path, "GET http://") {
		t.Fatalf("обращение в сеть не в плане: %+v", effects)
	}
	if err := box.AllowNet([]string{"https://x/y"}); err == nil {
		t.Fatal("--allow-net со схемой принят")
	}
}

// Встроенные функции с точкой проверяются до запуска, как остальные.
func TestCheckKnowsDottedBuiltins(t *testing.T) {
	file, err := Parse("<тест>", "print(time.now())\nprint(time.format(\"%Y\", now(), 1))\nprint(tme.now())\n")
	if err != nil {
		t.Fatal(err)
	}
	problems := Check(file, NewRegistry())
	if len(problems) != 2 {
		t.Fatalf("проблемы: %v", problems)
	}
	if !strings.Contains(problems[0].Msg, "time.format") || !strings.Contains(problems[1].Msg, "tme") {
		t.Fatalf("проблемы: %v", problems)
	}
}
