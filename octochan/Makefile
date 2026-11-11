# ochan: сборка и установка.
#
#   make build                   bin/ochan и bin/ochan-mcp в каталоге репозитория
#   make build OUT=$HOME/tools   туда, куда можно писать: системные каталоги не нужны
#   make install                 спросит, как ставить: всё в эту папку (быстрая) или каждый путь отдельно (ручная)
#   make install HERE=1          быстрая без вопросов: программы, рабочий каталог и модули — в этой папке
#   make install HERE=/путь      то же в другой папке
#   make install BIN_DIR=… HOME_DIR=… MODULES_DIR=… LOGS_DIR=… REPORTS_DIR=… REGISTRY_CACHE_DIR=…
#                                ручная без вопросов; незаданное — по умолчанию
#   make install HERE=1 DESKTOP=1    ещё и значок: меню приложений и рабочий стол
#   make install PREFIX=/usr/local   программы в PREFIX/bin (спросит sudo), остальное по умолчанию
#   make vendor                  зависимости в vendor/ — для сборки без сети
#   make offline                 собрать из vendor/, в сеть не обращаясь
#   make rlm                     ещё и модуль RLM
#   make llm-test                тест обучения своей модели (llama/train_own.py; нужен venv с requirements.txt)
#   make icon                    значок из логотипа: cmd/icon.png и ресурс Windows (после смены docs/assets/logo.png)
#   make dist                    релиз под все платформы в dist/
#
# Версия — VERSION; её печатают ochan --version и ochan-mcp --version.
# SYSTEM_ROLES — системная политика ролей (docs/guide/security.md): путь
# зашивается в программу, окружением его не поменять.
VERSION      ?= 0.3.0-alpha
# PREFIX пуст — куда ставить, спрашивает установка (ochan setup); удаление
# без PREFIX смотрит в ~/.local/bin.
PREFIX       ?=
OUT          ?= bin
SYSTEM_ROLES ?= /etc/octochan/roles.yaml

LDFLAGS     := -X ochan/cmd.Version=$(VERSION) -X ochan/rbac.SystemPolicyPath=$(SYSTEM_ROLES)
MCP_LDFLAGS := -X main.serverVersion=$(VERSION)
PLATFORMS   := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build install uninstall rlm llm-test vendor offline dist test vet version icon

PYTHON ?= python3

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o "$(abspath $(OUT))/ochan" .
	cd mcp && go build -trimpath -ldflags "$(MCP_LDFLAGS)" -o "$(abspath $(OUT))/ochan-mcp" .

# Ключи установки из переменных make: чего нет — о том спросит ochan setup.
SETUP_ARGS := $(if $(HERE),$(if $(filter 1 yes true,$(HERE)),--here,"--here=$(HERE)")) \
	$(if $(BIN_DIR),"--bin=$(BIN_DIR)") $(if $(HOME_DIR),"--home=$(HOME_DIR)") \
	$(if $(MODULES_DIR),"--modules=$(MODULES_DIR)") $(if $(LOGS_DIR),"--logs=$(LOGS_DIR)") \
	$(if $(REPORTS_DIR),"--reports=$(REPORTS_DIR)") $(if $(REGISTRY_CACHE_DIR),"--registry-cache=$(REGISTRY_CACHE_DIR)") \
	$(if $(DESKTOP),--desktop)

install:
	PREFIX="$(PREFIX)" VERSION="$(VERSION)" sh scripts/install.sh $(SETUP_ARGS)

uninstall:
	PREFIX="$(PREFIX)" sh scripts/install.sh --uninstall

rlm:
	PREFIX="$(PREFIX)" VERSION="$(VERSION)" sh scripts/install.sh --rlm $(SETUP_ARGS)

llm-test:
	cd llama && $(PYTHON) -m unittest discover -s tests -v

# Зависимости обоих модулей (ochan и ochan-mcp) — в один vendor/ рабочего
# пространства go.work. Каталог переносится вместе с исходниками туда, где
# сети нет.
vendor:
	go work vendor

offline:
	GOFLAGS=-mod=vendor GOPROXY=off $(MAKE) build

dist:
	@mkdir -p dist
	@for target in $(PLATFORMS); do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; [ "$$os" = windows ] && ext=.exe; \
		echo "ochan $(VERSION) $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w $(LDFLAGS)" \
			-o "dist/ochan-$(VERSION)-$$os-$$arch$$ext" . || exit 1; \
		(cd mcp && GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w $(MCP_LDFLAGS)" \
			-o "../dist/ochan-mcp-$(VERSION)-$$os-$$arch$$ext" .) || exit 1; \
	done
	cd dist && sha256sum * > SHA256SUMS

version:
	@echo $(VERSION)

# Значок программы — эмблема логотипа: PNG для меню приложений и ресурс
# Windows, который go build сам прилинковывает к ochan.exe.
icon:
	go run ./tools/icongen -in docs/assets/logo.png -png cmd/icon.png -syso rsrc_windows_amd64.syso

test:
	go test -race ./... ./mcp/...

vet:
	go vet ./... && cd mcp && go vet .
