// ochan-mcp — сервер Model Context Protocol для octochan.
//
// Это отдельный модуль, а не часть ochan: ядро о нём не знает и без него
// работает так же, как раньше. Сервер не содержит ни одной модели и не
// зависит ни от одного поставщика: MCP — открытый протокол, и подключиться
// к серверу может любой клиент с любой моделью.
//
// Сервер вызывает ochan через командную строку по закрытой таблице
// инструментов. Модель заполняет типизированные слоты, аргументы строит
// таблица, и всё, что не прошло проверку, отвергается до запуска. Команд,
// которые что-то выполняют (apply, approve, rollback, patch и другие), в
// таблице нет: единственное, что сервер может создать, — заявку, которая
// ждёт решения человека.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"
)

// listFlag — флаг, который можно указать несколько раз.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

func main() {
	os.Exit(run())
}

func run() int {
	var (
		ochanPath string
		roots     listFlag
		live      listFlag
		env       listFlag
		timeout   time.Duration
		maxOutput int
		version   bool
	)

	flags := flag.NewFlagSet("ochan-mcp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&ochanPath, "ochan", "", "путь к ochan (по умолчанию — рядом с сервером или из PATH)")
	flags.Var(&roots, "root", "каталог, внутри которого разрешены файлы; можно указать несколько раз (по умолчанию — текущий)")
	flags.Var(&live, "allow-live", "разрешить живые источники этой схемы, например postgres; по умолчанию запрещены")
	flags.Var(&env, "allow-env", "разрешить сценариям читать эту переменную окружения; по умолчанию ни одну")
	flags.DurationVar(&timeout, "timeout", 2*time.Minute, "предел времени одного вызова")
	flags.IntVar(&maxOutput, "max-output", 100_000, "предел объёма вывода, возвращаемого модели, в байтах")
	flags.BoolVar(&version, "version", false, "показать версию и выйти")

	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if version {
		// Версию печатаем в stderr: stdout принадлежит протоколу.
		fmt.Fprintf(os.Stderr, "ochan-mcp %s\n", serverVersion)
		return 0
	}

	logger := log.New(os.Stderr, "ochan-mcp: ", log.LstdFlags)

	if len(roots) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			logger.Printf("не удалось определить текущий каталог: %v", err)
			return 1
		}
		roots = listFlag{cwd}
	}

	box, err := newSandbox(roots, live, env...)
	if err != nil {
		logger.Print(err)
		return 1
	}

	binary, err := findOchan(ochanPath)
	if err != nil {
		logger.Print(err)
		return 1
	}

	srv, err := newServer(box, &runner{
		binary:    binary,
		dir:       box.roots[0],
		timeout:   timeout,
		maxOutput: maxOutput,
	}, os.Stdout, logger)
	if err != nil {
		logger.Print(err)
		return 1
	}

	logger.Printf("запущен: ochan %s, каталоги %s", binary, strings.Join(box.roots, ", "))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, os.Stdin) }()

	select {
	case err := <-done:
		if err != nil {
			logger.Printf("ошибка чтения: %v", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		// Чтение stdin прервать нельзя, поэтому не ждём его: запущенные
		// команды уже получили отмену, даём им завершиться и выходим.
		waited := make(chan struct{})
		go func() { srv.calls.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-time.After(10 * time.Second):
			logger.Print("не все команды завершились за 10 с")
		}
		return 130
	}
}
