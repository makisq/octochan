package cmd

import (
	"ochan/core/scenario"
	"ochan/modules/httpexec"
)

// Исполнитель http — в ядре: запрос к API узла (POST и другие глаголы) с
// ответом для f @ (…). Токен — ochan auth --executor http, настройки —
// executors.http в config.yaml; берутся при каждом вызове.
func init() {
	scenario.Register(httpexec.Service, httpexec.New(func() (string, map[string]string) {
		supply := executorSupply("http")
		return supply.Credentials["token"], supply.Settings
	}))
}
