#!/bin/sh
# Собирает ochan и ochan-mcp и кладёт их в каталог из PATH: после этого
# они вызываются по имени, как echo или python, — без ./ и пути.
#
#   scripts/install.sh                  в ~/.local/bin
#   PREFIX=/usr/local scripts/install.sh    в /usr/local/bin (спросит sudo)
#   scripts/install.sh --rlm            ещё и модуль RLM
#   scripts/install.sh --llm            ещё и адаптер своей модели ℒ: llama/*.py
#                                       в $PREFIX/share/ochan/llm, venv с requirements.txt
#                                       (PYTHON — интерпретатор, по умолчанию python3;
#                                       TORCH_INDEX — индекс pip для torch, например
#                                       https://download.pytorch.org/whl/cu128)
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

WITH_RLM=
WITH_LLM=
for arg in "$@"; do
	case "$arg" in
	--uninstall)
		remove
		exit 0
		;;
	--rlm) WITH_RLM=1 ;;
	--llm) WITH_LLM=1 ;;
	*)
		echo "Неизвестный ключ $arg: --rlm, --llm, --uninstall" >&2
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

if [ -n "$WITH_LLM" ]; then
	# Адаптер своей модели ℒ: ochan находит его сам — adapter.py в
	# share/ochan/llm рядом с bin, интерпретатор — venv рядом с ним.
	LLM="$PREFIX/share/ochan/llm"
	PYTHON=${PYTHON:-python3}
	command -v "$PYTHON" >/dev/null 2>&1 || { echo "Нужен Python 3.10 или новее: PYTHON=путь" >&2; exit 1; }
	mkdir -p "$LLM" 2>/dev/null || sudo mkdir -p "$LLM"
	if [ -w "$LLM" ]; then SUDO=; else SUDO=sudo; fi
	$SUDO cp "$ROOT"/llama/*.py "$ROOT/llama/requirements.txt" "$LLM/"
	echo "Сборка fpcheck…"
	(cd "$ROOT" && go build -trimpath -o "$OUT/fpcheck" ./llama/fpcheck)
	$SUDO install -m 0755 "$OUT/fpcheck" "$LLM/"
	if [ ! -x "$LLM/venv/bin/python" ]; then
		echo "Окружение Python: $LLM/venv"
		$SUDO "$PYTHON" -m venv "$LLM/venv"
	fi
	if [ -n "${TORCH_INDEX:-}" ]; then
		$SUDO "$LLM/venv/bin/python" -m pip install torch --index-url "$TORCH_INDEX"
	fi
	$SUDO "$LLM/venv/bin/python" -m pip install -r "$LLM/requirements.txt"
	echo "Адаптер своей модели: $LLM/adapter.py; в config.yaml — llm.model (каталог модели) и llm.endpoint, запуск — ochan llm serve"
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
