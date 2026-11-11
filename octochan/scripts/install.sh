#!/bin/sh
# Собирает ochan и ochan-mcp и ставит их. Куда — решаете вы.
#
#   scripts/install.sh                  спросит: всё в эту папку (быстрая) или каждый путь отдельно (ручная)
#   scripts/install.sh --here           быстрая без вопросов: программы, рабочий каталог и модули — в текущей папке
#   scripts/install.sh --here=ПАПКА     то же в другой папке
#   scripts/install.sh --bin=ПАПКА --home=ПАПКА --modules=ПАПКА --logs=ПАПКА --reports=ПАПКА --registry-cache=ПАПКА
#                                       ручная без вопросов; незаданное — по умолчанию
#   scripts/install.sh --here --desktop ещё и значок: меню приложений и рабочий стол
#   PREFIX=/usr/local scripts/install.sh    программы в PREFIX/bin (спросит sudo), остальное по умолчанию
#   scripts/install.sh --rlm            ещё и модуль RLM
#   scripts/install.sh --uninstall      убрать программы из PREFIX/bin (по умолчанию ~/.local/bin)
#
# Быстрая установка — для машин, где работать и запускать программы
# разрешено только в одной папке: рядом с ochan остаётся указатель
# ochan.home, и переменные окружения не нужны. Вопросы задаёт сам ochan
# (ochan setup) и читает ответы из stdin построчно: их можно подать и
# конвейером, а без ответа ставит в каталоги по умолчанию.
#
# Нужен Go 1.22 или новее. Сборка идёт от вашего имени, sudo — только на
# копирование в PREFIX/bin, если каталог вам не принадлежит.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
LEGACY_PREFIX=${PREFIX:-}
BIN="${LEGACY_PREFIX:-$HOME/.local}/bin"

copy() {
	if [ -w "$BIN" ] || { [ ! -e "$BIN" ] && mkdir -p "$BIN" 2>/dev/null; }; then
		install -m 0755 "$@" "$BIN/"
	elif command -v sudo >/dev/null 2>&1; then
		echo "Каталог $BIN не ваш: копирую через sudo"
		sudo mkdir -p "$BIN"
		sudo install -m 0755 "$@" "$BIN/"
	else
		echo "Нет прав на запись в $BIN и нет sudo; укажите PREFIX или ставьте в папку: --here" >&2
		exit 1
	fi
}

remove() {
	# Значок убирает сама программа, пока она есть.
	if [ -x "$BIN/ochan" ]; then "$BIN/ochan" setup --desktop=remove || true; fi
	for name in ochan ochan-mcp; do
		[ -e "$BIN/$name" ] || continue
		if [ -w "$BIN" ]; then rm -f "$BIN/$name"; else sudo rm -f "$BIN/$name"; fi
		echo "Удалён $BIN/$name"
	done
}

WITH_RLM=
SETUP=
for arg; do
	shift
	case "$arg" in
	--uninstall)
		remove
		exit 0
		;;
	--rlm) WITH_RLM=1 ;;
	--here | --here=* | --bin=* | --home=* | --modules=* | --logs=* | --reports=* | --registry-cache=* | --desktop | --desktop=* | --yes | -y)
		SETUP=1
		set -- "$@" "$arg"
		;;
	*)
		echo "Неизвестный ключ $arg: --here[=папка], --bin=, --home=, --modules=, --logs=, --reports=, --registry-cache=, --desktop, --yes, --rlm, --uninstall" >&2
		exit 1
		;;
	esac
done

command -v go >/dev/null 2>&1 || { echo "Нужен Go 1.22 или новее: https://go.dev/dl/" >&2; exit 1; }

OUT="$ROOT/bin"
mkdir -p "$OUT"
# VERSION задаёт make install; без неё — версия, записанная в исходниках.
LDFLAGS=""
MCP_LDFLAGS=""
if [ -n "${VERSION:-}" ]; then
	LDFLAGS="-X ochan/cmd.Version=$VERSION"
	MCP_LDFLAGS="-X main.serverVersion=$VERSION"
fi
echo "Сборка ochan…"
(cd "$ROOT" && go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/ochan" .)
echo "Сборка ochan-mcp…"
(cd "$ROOT/mcp" && go build -trimpath -ldflags "$MCP_LDFLAGS" -o "$OUT/ochan-mcp" .)
if [ -n "$WITH_RLM" ]; then
	echo "Сборка модуля RLM…"
	(cd "$ROOT" && go build -trimpath -o "$OUT/rlm.hcplugin" ./plugins/rlm)
fi

if [ -z "$LEGACY_PREFIX" ] || [ -n "$SETUP" ]; then
	# Куда ставить, решает ochan setup: спрашивает или берёт из ключей.
	if [ -n "$LEGACY_PREFIX" ]; then
		echo "PREFIX вместе с ключами установки не действует: каталог программ — --bin=ПАПКА" >&2
	fi
	if [ -n "$WITH_RLM" ]; then
		# Модуль ставит сам ochan: он фиксирует контрольную сумму, и
		# подменённый файл модуля потом не загрузится.
		set -- "$@" "--module=$OUT/rlm.hcplugin"
	fi
	"$OUT/ochan" setup "$@"
	if [ -n "$WITH_RLM" ]; then
		echo "Токен RLM: ochan auth --executor rlm; адрес — executors.rlm.api_url в config.yaml"
	fi
	exit 0
fi

# PREFIX задан: программы — в PREFIX/bin, как у обычных программ системы.
copy "$OUT/ochan" "$OUT/ochan-mcp"
echo "Установлено: $BIN/ochan, $BIN/ochan-mcp"

if [ -n "$WITH_RLM" ]; then
	"$BIN/ochan" '!!install' "$OUT/rlm.hcplugin"
	echo "Токен RLM: ochan auth --executor rlm; адрес — executors.rlm.api_url в ~/.octochan/config.yaml"
fi

case ":$PATH:" in
*":$BIN:"*)
	"$BIN/ochan" --version
	;;
*)
	# Каталога нет в PATH: говорим, какую строку добавить, но профиль
	# оболочки сами не правим — это решение пользователя.
	profile="$HOME/.profile"
	case "${SHELL:-}" in
	*/zsh) profile="$HOME/.zshrc" ;;
	*/bash) profile="$HOME/.bashrc" ;;
	*/fish) profile="$HOME/.config/fish/config.fish" ;;
	esac
	echo
	echo "Каталога $BIN нет в PATH. Добавьте его и откройте терминал заново:"
	case "$profile" in
	*.fish) echo "  fish_add_path $BIN" ;;
	*) echo "  echo 'export PATH=\"$BIN:\$PATH\"' >> $profile" ;;
	esac
	;;
esac
