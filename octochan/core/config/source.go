package config

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// LoadedConfig — конфиг вместе с описанием источника.
type LoadedConfig struct {
	// Name — человекочитаемое имя источника. Никогда не содержит пароль:
	// оно попадает в отчёты, журнал и сообщения об ошибках.
	Name string
	Data ConfigMap
	// Notes — замечания источника, например список параметров,
	// которым для применения нужен перезапуск.
	Notes []string
}

// SourceOptions управляет загрузкой.
type SourceOptions struct {
	// IncludeDefaults включает параметры, оставленные в значении по умолчанию.
	// По умолчанию выключено: сравнение живого сервера с небольшим conf-файлом
	// иначе утонет в трёх сотнях строк «есть только слева».
	IncludeDefaults bool
}

// SourceLoader читает конфиг из источника своей схемы.
type SourceLoader func(ctx context.Context, ref string, opts SourceOptions) (*LoadedConfig, error)

// Реестр схем позволяет держать движок сравнения свободным от драйверов:
// core не знает про PostgreSQL, а пакет источника регистрируется сам.
var (
	sourceMu      sync.RWMutex
	sourceLoaders = make(map[string]SourceLoader)
)

// RegisterSourceScheme регистрирует загрузчик для схемы вида "postgres".
func RegisterSourceScheme(scheme string, loader SourceLoader) {
	sourceMu.Lock()
	defer sourceMu.Unlock()
	sourceLoaders[strings.ToLower(scheme)] = loader
}

// RegisteredSourceSchemes перечисляет доступные схемы.
func RegisteredSourceSchemes() []string {
	sourceMu.RLock()
	defer sourceMu.RUnlock()

	schemes := make([]string, 0, len(sourceLoaders))
	for scheme := range sourceLoaders {
		schemes = append(schemes, scheme)
	}
	sort.Strings(schemes)
	return schemes
}

// SourceScheme возвращает схему ссылки: для "postgres://host/db" это
// "postgres", для обычного пути — пустая строка.
//
// Путь вида C:\dir проверяется отдельно: одиночная буква перед двоеточием
// это диск Windows, а не схема.
func SourceScheme(ref string) string {
	scheme, rest, found := strings.Cut(ref, "://")
	if !found || scheme == "" || rest == "" {
		return ""
	}
	if len(scheme) == 1 {
		return ""
	}
	for _, r := range scheme {
		isLetter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !isLetter && r != '+' && r != '-' && r != '.' {
			return ""
		}
	}
	return strings.ToLower(scheme)
}

// LoadConfigFrom загружает конфиг из файла или зарегистрированного источника.
//
// Это единственная точка, через которую конфиг попадает в инструмент,
// поэтому живой сервер работает всюду, где раньше принимался только путь.
func LoadConfigFrom(ctx context.Context, ref string, opts SourceOptions) (*LoadedConfig, error) {
	if scheme := SourceScheme(ref); scheme != "" {
		sourceMu.RLock()
		loader, ok := sourceLoaders[scheme]
		sourceMu.RUnlock()

		if !ok {
			known := RegisteredSourceSchemes()
			if len(known) == 0 {
				return nil, fmt.Errorf("источник %q не поддерживается: схем не зарегистрировано", scheme)
			}
			return nil, fmt.Errorf("источник %q не поддерживается; доступны: %s",
				scheme, strings.Join(known, ", "))
		}
		return loader(ctx, ref, opts)
	}

	data, err := os.ReadFile(ref)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать %s: %w", ref, err)
	}

	cfg, err := ParseConfig(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	return &LoadedConfig{Name: ref, Data: cfg}, nil
}

// LoadConfigFromText разбирает уже прочитанный текст.
func LoadConfigFromText(name, content string) (*LoadedConfig, error) {
	cfg, err := ParseConfig(content)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &LoadedConfig{Name: name, Data: cfg}, nil
}
