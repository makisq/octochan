// Демонстрационный движок встроенной модели — пример протокола движка v1
// и заготовка адаптера.
//
// Настоящий адаптер (llama.cpp, vLLM, своя LLaMA на fp) делает то же:
// отвечает на GET /v1/info и POST /v1/next — распределение следующего
// токена после prompt+output, top самых вероятных и вероятность конца
// ответа (embedded/http.go). Этот движок знаний не имеет: он ведёт к
// ответу из файла и по дороге пробует заговорить на естественном языке —
// чтобы было видно, что кольцо 0 и маска языка не пропускают этого в ответ.
//
// Сборка и запуск:
//
//	go build -o embedded-demo ./examples/embeddedengine
//	./embedded-demo --answer answer.fp --chatter "0=Sure, here is the report" --listen 127.0.0.1:8080
//
// В config.yaml ochan:
//
//	llm:
//	  mode: embedded
//	  endpoint: http://127.0.0.1:8080
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"octochan/embedded"
)

type chatterFlag map[int]string

func (c chatterFlag) String() string { return fmt.Sprint(map[int]string(c)) }

func (c chatterFlag) Set(v string) error {
	n, text, ok := strings.Cut(v, "=")
	i, err := strconv.Atoi(n)
	if !ok || err != nil || i < 0 {
		return fmt.Errorf("--chatter номер=фраза: номер строки ответа с нуля")
	}
	c[i] = text
	return nil
}

func main() {
	answer := flag.String("answer", "", "файл с ответом, к которому ведёт демонстрационная модель")
	listen := flag.String("listen", "127.0.0.1:8080", "адрес")
	chatter := chatterFlag{}
	flag.Var(chatter, "chatter", "номер=фраза: в начале этой строки ответа (с нуля) модель пробует сказать фразу; можно несколько")
	flag.Parse()
	if *answer == "" {
		log.Fatal("--answer: файл с ответом")
	}
	data, err := os.ReadFile(*answer)
	if err != nil {
		log.Fatal(err)
	}
	model := &embedded.Demo{Answer: string(data), Chatter: chatter}
	info := embedded.Info{Engine: "ochan-demo", Model: "demo (" + *answer + ")"}
	srv := &http.Server{Addr: *listen, Handler: embedded.Handler(info, model), ReadHeaderTimeout: 10 * time.Second}
	fmt.Printf("Демонстрационный движок: http://%s, протокол v%d; ответ — %s; знаний у модели нет\n",
		*listen, embedded.Protocol, *answer)
	log.Fatal(srv.ListenAndServe())
}
