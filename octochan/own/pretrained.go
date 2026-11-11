package own

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Стартовая модель ℒ — обученные веса, которые доставляются вместе с ochan.
//
// Учить язык с нуля на каждой машине незачем: язык один, и веса, которые
// его выучили, — тоже. Они лежат в репозитории (own/pretrained) и встроены
// в бинарник: ochan llm init --pretrained пишет их в каталог модели — и
// модель отвечает сразу, без Python и видеокарты, в том числе на машине без
// сети. Своё у каждой установки — не веса, а параметры колец: надёжность
// правил по её журналу и инцидентам (ochan llm fit, ochan llm learn); со
// стартовыми весами их нет — холодный старт с условным счётом автора.
//
// Веса — int8: ответы те же, что у полных (тот же токен в 99,99 % позиций
// ответа), а места вчетверо меньше. Для дообучения сети их распаковывают
// в float32 (full): обучение идёт с полных весов.

//go:embed pretrained/config.json pretrained/fpmtok.json pretrained/quant.json pretrained/quant.safetensors
var pretrained embed.FS

// PretrainedName — какая модель встроена.
const PretrainedName = "own-small-2, int8"

var pretrainedFiles = []string{ConfigFile, TokenizerFile, quantJSON, quantWeights}

// WritePretrained пишет стартовую модель в каталог dir. full — веса
// распаковываются в float32 (model.safetensors): с них идёт дообучение сети.
func WritePretrained(dir string, full bool) error {
	if IsModel(dir) {
		return fmt.Errorf("%s — уже модель: стартовая — в новый каталог, прежняя остаётся", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range pretrainedFiles {
		data, err := pretrained.ReadFile("pretrained/" + name)
		if err != nil {
			return fmt.Errorf("стартовая модель: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	if !full {
		return nil
	}
	weights, err := readQuantized(dir)
	if err != nil {
		return fmt.Errorf("стартовая модель: %w", err)
	}
	// Масштабы групп — служебные тензоры квантования: полным весам они не
	// нужны, а torch счёл бы их лишними.
	for name := range weights {
		if strings.HasSuffix(name, ".scale") || strings.HasSuffix(name, ".q") {
			delete(weights, name)
		}
	}
	if err := WriteSafetensors(filepath.Join(dir, "model.safetensors"), weights); err != nil {
		return err
	}
	for _, name := range []string{quantJSON, quantWeights} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
