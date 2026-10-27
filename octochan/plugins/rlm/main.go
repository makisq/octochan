// Модуль RLM для ochan: исполнитель сценариев через RLM API.
//
// RLM — внешняя система автоматизации. До 0.2.0 её исполнитель был встроен
// в ядро; теперь это обычный модуль: ядро выбирает его по полю service
// сценария, как любого исполнителя, и передаёт ему токен и настройки.
//
// Сборка и установка — make rlm или:
//
//	go build -o rlm.hcplugin ./plugins/rlm
//	ochan '!install' rlm.hcplugin
//
// Настройки — в config.yaml ochan; адрес можно оставить и прежним,
// defaults.api_url:
//
//	executors:
//	  rlm:
//	    api_url: https://rlm.example/api/tasks.json
//	    http_timeout: 30s
//
// Токен — ochan auth --executor rlm или OCTOCHAN_TOKEN_RLM. Прежние
// ~/.octochan/.env и OCTOCHAN_API_TOKEN модуль тоже понимает.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"octochan/config"
	_ "octochan/module" // сервисы RLM: pangolin_restart, psql_tuning_params_se…
	"octochan/pluginhost"
	"octochan/rlm"
	"octochan/runnerhost"
	"octochan/scenario"

	"github.com/spf13/cobra"
)

// configured — исполнитель RLM с настройками и токеном из вызова: своего
// config.yaml у процесса модуля нет.
type configured struct {
	inner scenario.Runner
}

func (c configured) prepare(ctx context.Context) {
	rlm.ConfigureFrom(runnerhost.Settings(ctx))
	if token := runnerhost.Token(ctx); token != "" {
		rlm.SetAPIToken(token)
	}
}

func (c configured) Kind() string { return c.inner.Kind() }

func (c configured) Plan(ctx context.Context, s *scenario.Scenario) (*config.Plan, error) {
	c.prepare(ctx)
	return c.inner.Plan(ctx, s)
}

func (c configured) Apply(ctx context.Context, s *scenario.Scenario) (*scenario.Result, error) {
	c.prepare(ctx)
	return c.inner.Apply(ctx, s)
}

func (c configured) Wait(ctx context.Context, ref string) (string, error) {
	c.prepare(ctx)
	return c.inner.(scenario.Waiter).Wait(ctx, ref)
}

func (c configured) Status(ctx context.Context, ref string) (*scenario.Status, error) {
	c.prepare(ctx)
	return c.inner.(scenario.StatusReporter).Status(ctx, ref)
}

// module — модуль ochan: команд и языков нет, есть исполнитель RLM.
type module struct{}

func (module) GetCommands() []*cobra.Command { return nil }
func (module) Init(map[string]any) error     { return nil }
func (module) Name() string                  { return "rlm" }
func (module) Version() string               { return "0.2.0" }

func (module) Runners() []pluginhost.RunnerSpec {
	services := scenario.Services()
	if len(services) == 0 {
		return nil
	}
	runner, err := scenario.Resolve(services[0])
	if err != nil {
		return nil
	}
	return []pluginhost.RunnerSpec{runnerhost.Expose(configured{inner: runner}, services...)}
}

func main() {
	// Токен, который до 0.2.0 сохранял ochan auth.
	if home, err := os.UserHomeDir(); err == nil {
		if err := rlm.LoadTokenFrom(filepath.Join(home, ".octochan", ".env")); err != nil {
			fmt.Fprintf(os.Stderr, "rlm: прежний токен ~/.octochan/.env не прочитан: %v\n", err)
		}
	}
	pluginhost.ServeCommandModule(module{})
}
