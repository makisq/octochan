// Package i18n — язык, на котором ochan говорит с человеком.
//
// Исходный язык — русский: русский текст и есть ключ (как msgid в
// gettext). Перевод — словарь «русский текст → перевод». Английский
// встроен, остальные языки приносят модули: так немецкий или японский
// ставится как любой другой модуль и не требует пересборки ochan.
//
// Чего нет в словаре языка, берётся из английского, а чего нет и там,
// остаётся по-русски: недостающий перевод — строка на другом языке, а не
// пустое место и не ошибка.
//
// Пакет без зависимостей: он нужен всем остальным, а сам не нужен
// никому.
package i18n

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Source — исходный язык.
const Source = "ru"

// Language — язык: код, название и словарь.
type Language struct {
	// Code — код языка из конфига: en, de, jp.
	Code string
	// Name — название языка на нём самом: English, Deutsch, 日本語.
	Name string
	// Origin — модуль, который принёс язык; пусто — язык встроен.
	Origin string
	// Entries — русский текст → перевод.
	Entries map[string]string
}

var (
	mu        sync.RWMutex
	languages = map[string]*Language{
		Source: {Code: Source, Name: "Русский"},
		"en":   {Code: "en", Name: "English", Entries: english},
	}
	current = languages[Source]
)

// aliases — другие написания кодов встроенных языков: в конфиге пишут
// и lang: eng, и lang: english.
var aliases = map[string]string{
	"ru": "ru", "rus": "ru", "russian": "ru", "русский": "ru",
	"en": "en", "eng": "en", "english": "en",
}

// Normalize приводит код языка к виду, под которым он зарегистрирован.
func Normalize(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if canonical, ok := aliases[code]; ok {
		return canonical
	}
	return code
}

// Register добавляет язык. Язык с тем же кодом дополняется: модуль может
// перевести и команды других модулей. Перевести исходный язык нельзя.
func Register(lang Language) error {
	code := Normalize(lang.Code)
	switch {
	case code == "":
		return fmt.Errorf("у языка нет кода")
	case code == Source:
		return fmt.Errorf("русский — исходный язык ochan, переводить его не на что")
	case strings.ContainsAny(code, " /\\\t\n"):
		return fmt.Errorf("недопустимый код языка %q", lang.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	have, ok := languages[code]
	if !ok {
		entries := make(map[string]string, len(lang.Entries))
		for k, v := range lang.Entries {
			entries[k] = v
		}
		languages[code] = &Language{Code: code, Name: lang.Name, Origin: lang.Origin, Entries: entries}
		return nil
	}

	// Встроенный словарь не меняется: модуль дописывает только то, чего
	// в нём нет.
	merged := make(map[string]string, len(have.Entries)+len(lang.Entries))
	for k, v := range lang.Entries {
		merged[k] = v
	}
	for k, v := range have.Entries {
		merged[k] = v
	}
	have.Entries = merged
	switch {
	case lang.Origin == "" || strings.Contains(have.Origin, lang.Origin):
	case have.Origin == "":
		have.Origin = lang.Origin
	default:
		have.Origin += ", " + lang.Origin
	}
	return nil
}

// Use делает язык текущим. Неизвестный язык — ошибка, и текущим остаётся
// прежний.
func Use(code string) error {
	code = Normalize(code)
	mu.Lock()
	defer mu.Unlock()
	lang, ok := languages[code]
	if !ok {
		return fmt.Errorf("язык %q не найден; доступны: %s. Другой язык ставится модулем: ochan !!install <модуль>",
			code, strings.Join(codesLocked(), ", "))
	}
	current = lang
	return nil
}

// Current возвращает код текущего языка.
func Current() string {
	mu.RLock()
	defer mu.RUnlock()
	return current.Code
}

// Languages возвращает языки, отсортированные по коду.
func Languages() []Language {
	mu.RLock()
	defer mu.RUnlock()
	var out []Language
	for _, code := range codesLocked() {
		out = append(out, *languages[code])
	}
	return out
}

// Lookup возвращает перевод текста на язык code и есть ли он.
func Lookup(code, text string) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	lang, ok := languages[Normalize(code)]
	if !ok {
		return "", false
	}
	if lang.Code == Source {
		return text, true
	}
	translated, ok := lang.Entries[text]
	return translated, ok
}

func codesLocked() []string {
	codes := make([]string, 0, len(languages))
	for code := range languages {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// Messages — тексты сообщений, известные встроенному словарю: вместе
// со справкой команд это шаблон для перевода на новый язык.
func Messages() []string {
	out := make([]string, 0, len(english))
	for text := range english {
		out = append(out, text)
	}
	sort.Strings(out)
	return out
}

// T переводит текст на текущий язык. Чего нет в языке модуля, берётся
// из английского: немцу понятнее английская строка, чем русская. Нет и
// в английском — остаётся по-русски.
func T(text string) string {
	mu.RLock()
	defer mu.RUnlock()
	if current.Code == Source || text == "" {
		return text
	}
	if translated, ok := current.Entries[text]; ok {
		return translated
	}
	if translated, ok := english[text]; ok {
		return translated
	}
	return text
}

// Sprintf переводит формат и подставляет значения.
func Sprintf(format string, args ...any) string {
	return fmt.Sprintf(T(format), args...)
}

// Errorf — fmt.Errorf с переведённым форматом; %w работает как обычно.
func Errorf(format string, args ...any) error {
	return fmt.Errorf(T(format), args...)
}
