// Package pgsource читает конфигурацию живого экземпляра PostgreSQL.
//
// Для работающего сервера postgresql.conf не является источником правды:
// поверх него действуют ALTER SYSTEM (postgresql.auto.conf), директивы
// include и параметры командной строки. Два сервера могут иметь одинаковые
// conf-файлы и вести себя по-разному. Авторитетный ответ даёт pg_settings,
// его и читаем.
//
// Пакет держится отдельно от core намеренно: движок сравнения конфигов
// не должен тянуть за собой драйвер базы данных.
package pgsource

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"ochan/config"

	"github.com/jackc/pgx/v5"
)

func init() {
	config.RegisterSourceScheme("postgres", Load)
	config.RegisterSourceScheme("postgresql", Load)
}

// settingsQuery берёт только то, что действительно является конфигурацией.
//
// Фильтр по source отсекает три вида шума, и все три обнаружились
// при сравнении с настоящим сервером:
//
//   - default: около трёхсот параметров, оставленных в значении по умолчанию;
//     рядом с обычным postgresql.conf они дают простыню «есть только справа»;
//   - override: не конфигурация, а факты рантайма — config_file,
//     data_directory, hba_file, а также transaction_isolation
//     и остальное состояние текущей транзакции;
//   - client и session: параметры, выставленные самим подключением.
//     Без этого фильтра инструмент показывал расхождение по application_name,
//     которое сам же и создавал.
//
// Показать всё целиком можно через SourceOptions.IncludeDefaults.
const settingsQuery = `
SELECT name,
       setting,
       COALESCE(unit, ''),
       source,
       pending_restart
FROM pg_settings
WHERE $1 OR (source <> 'default'
             AND source NOT IN ('override', 'client', 'session'))
ORDER BY name`

// Load подключается к серверу и читает его действующую конфигурацию.
func Load(ctx context.Context, dsn string, opts config.SourceOptions) (*config.LoadedConfig, error) {
	connConfig, display, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}

	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return nil, fmt.Errorf("не удалось подключиться к %s: %w", display, err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	rows, err := conn.Query(ctx, settingsQuery, opts.IncludeDefaults)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать pg_settings на %s: %w", display, err)
	}
	defer rows.Close()

	params := make(map[string]string)
	var pendingRestart []string

	for rows.Next() {
		var (
			name, setting, unit, source string
			pending                     bool
		)
		if err := rows.Scan(&name, &setting, &unit, &source, &pending); err != nil {
			return nil, fmt.Errorf("не удалось разобрать строку pg_settings: %w", err)
		}

		params[name] = FormatSetting(setting, unit)
		if pending {
			pendingRestart = append(pendingRestart, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка чтения pg_settings на %s: %w", display, err)
	}

	loaded := &config.LoadedConfig{
		Name: display,
		// В postgresql.conf секций нет, поэтому всё кладём в безымянную —
		// так живой сервер и файл сравниваются напрямую.
		Data: config.ConfigMap{"": params},
	}

	if len(pendingRestart) > 0 {
		sort.Strings(pendingRestart)
		loaded.Notes = append(loaded.Notes, fmt.Sprintf(
			"изменены, но требуют перезапуска: %s", strings.Join(pendingRestart, ", ")))
	}

	return loaded, nil
}

// parseDSN разбирает строку подключения и возвращает её безопасное
// представление для вывода.
//
// Пароль в отображаемое имя не попадает никогда: это имя уходит в отчёты,
// журнал и сообщения об ошибках.
func parseDSN(dsn string) (*pgx.ConnConfig, string, error) {
	cleaned, err := stripOwnParams(dsn)
	if err != nil {
		return nil, "", err
	}

	connConfig, err := pgx.ParseConfig(cleaned)
	if err != nil {
		return nil, "", fmt.Errorf("некорректная строка подключения: %w%s", err, passwordHint(dsn, err))
	}

	// Подписываем подключение, чтобы в pg_stat_activity было видно, кто пришёл.
	// Администратору боевого сервера это важнее, чем нам.
	if connConfig.RuntimeParams == nil {
		connConfig.RuntimeParams = make(map[string]string)
	}
	if _, set := connConfig.RuntimeParams["application_name"]; !set {
		connConfig.RuntimeParams["application_name"] = "octochan"
	}

	display := fmt.Sprintf("postgres://%s@%s:%d/%s",
		connConfig.User, connConfig.Host, connConfig.Port, connConfig.Database)
	return connConfig, display, nil
}

// passwordHint подсказывает, что делать с паролем, который не разбирается
// как часть URL.
//
// Пароль со спецсимволами или кириллицей ломает разбор строки подключения,
// и сообщение net/url про "invalid userinfo" ничего не объясняет. Это частая
// заноза, поэтому объясняем прямо.
func passwordHint(dsn string, err error) string {
	if !strings.Contains(err.Error(), "userinfo") || !strings.Contains(dsn, "@") {
		return ""
	}
	return "\nпароль в строке подключения должен быть percent-encoded; " +
		"надёжнее не указывать его вовсе, а задать переменную PGPASSWORD " +
		"или прописать сервер в ~/.pgpass"
}

// stripOwnParams убирает из строки подключения параметры октохана.
// Неизвестные параметры pgx отправляет серверу, и тот отвечает ошибкой.
func stripOwnParams(dsn string) (string, error) {
	if !strings.Contains(dsn, "://") {
		return dsn, nil
	}

	parsed, err := url.Parse(dsn)
	if err != nil {
		// Разбирать URL — не наша задача: пусть об этом отчитается pgx,
		// у него сообщение точнее. Здесь просто ничего не трогаем.
		return dsn, nil
	}

	query := parsed.Query()
	for key := range query {
		if strings.HasPrefix(key, "ochan_") {
			query.Del(key)
		}
	}
	parsed.RawQuery = query.Encode()

	return parsed.String(), nil
}

// FormatSetting приводит пару (setting, unit) из pg_settings к тому виду,
// в каком значение пишут в postgresql.conf.
//
// Это самая тонкая часть: pg_settings хранит число в единицах поля unit,
// и unit может содержать множитель. Для shared_buffers сервер отдаёт
// setting=16384 при unit=8kB, то есть 131072kB. Без пересчёта сравнение
// живого сервера с файлом давало бы мусор в каждой строке.
func FormatSetting(setting, unit string) string {
	unit = strings.TrimSpace(unit)
	if unit == "" {
		return setting
	}

	// Нечисловые значения единиц не имеют: on, logical, $libdir/plugins.
	value, err := strconv.ParseFloat(setting, 64)
	if err != nil {
		return setting
	}

	multiplier, base := splitUnit(unit)
	total := value * float64(multiplier)

	if total == float64(int64(total)) {
		return strconv.FormatInt(int64(total), 10) + base
	}
	return strconv.FormatFloat(total, 'f', -1, 64) + base
}

// splitUnit разбивает "8kB" на множитель 8 и базовую единицу "kB".
// Единица без множителя ("ms", "kB") даёт множитель 1.
func splitUnit(unit string) (int64, string) {
	i := 0
	for i < len(unit) && unit[i] >= '0' && unit[i] <= '9' {
		i++
	}
	if i == 0 {
		return 1, unit
	}

	multiplier, err := strconv.ParseInt(unit[:i], 10, 64)
	if err != nil || multiplier == 0 {
		return 1, unit[i:]
	}
	return multiplier, unit[i:]
}
