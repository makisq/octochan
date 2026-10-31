#!/bin/sh
# Собирает ochan и ochan-mcp и кладёт их в каталог из PATH: после этого
# они вызываются по имени, как echo или python, — без ./ и пути.
#
#   scripts/install.sh                  в ~/.local/bin
#   PREFIX=/usr/local scripts/install.sh    в /usr/local/bin (спросит sudo)
#   scripts/install.sh --rlm            ещё и модуль RLM
#   scripts/install.sh --uninstall      убрать
#
# Нужен Go 1.22 или новее. Сборка идёт от вашего имени, sudo — только на
# копирование, если каталог вам не принадлежит.
set -eu

PREFIX=${PREFIX:-"$HOME/.local"}
BIN="$PREFIX/bin"
ROOT=$(cd "$(dirname "$0")/.." && pwd)

copy() {
	if [ -w "$BIN" ] || { [ ! -e "$BIN" ] && mkdir -p "$BIN" 2>/dev/null; }; then
		install -m 0755 "$@" "$BIN/"
	elif command -v sudo >/dev/null 2>&1; then
		echo "Каталог $BIN не ваш: копирую через sudo"
		sudo mkdir -p "$BIN"
		sudo install -m 0755 "$@" "$BIN/"
	else
		echo "Нет прав на запись в $BIN и нет sudo; укажите PREFIX" >&2
		exit 1
	fi
}

remove() {
	for name in ochan ochan-mcp; do
		[ -e "$BIN/$name" ] || continue
		if [ -w "$BIN" ]; then rm -f "$BIN/$name"; else sudo rm -f "$BIN/$name"; fi
		echo "Удалён $BIN/$name"
	done
}

if [ "${1:-}" = "--uninstall" ]; then
	remove
	exit 0
fi
WITH_RLM=
if [ "${1:-}" = "--rlm" ]; then
	WITH_RLM=1
fi

command -v go >/dev/null 2>&1 || { echo "Нужен Go 1.22 или новее: https://go.dev/dl/" >&2; exit 1; }

OUT="$ROOT/bin"
mkdir -p "$OUT"
# VERSION задаёт make install; без неё — версия, записанная в исходниках.
LDFLAGS=""
MCP_LDFLAGS=""
if [ -n "${VERSION:-}" ]; then
	LDFLAGS="-X octochan/cmd.Version=$VERSION"
	MCP_LDFLAGS="-X main.serverVersion=$VERSION"
fi
echo "Сборка ochan…"
(cd "$ROOT" && go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/ochan" .)
echo "Сборка ochan-mcp…"
(cd "$ROOT/mcp" && go build -trimpath -ldflags "$MCP_LDFLAGS" -o "$OUT/ochan-mcp" .)
copy "$OUT/ochan" "$OUT/ochan-mcp"
echo "Установлено: $BIN/ochan, $BIN/ochan-mcp"

if [ -n "$WITH_RLM" ]; then
	# Модуль ставит сам ochan: он фиксирует контрольную сумму, и подменённый
	# файл модуля потом не загрузится.
	echo "Сборка модуля RLM…"
	(cd "$ROOT" && go build -trimpath -o "$OUT/rlm.hcplugin" ./plugins/rlm)
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
