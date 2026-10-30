# ochan: сборка и установка.
#
#   make build                   bin/ochan и bin/ochan-mcp в каталоге репозитория
#   make build OUT=$HOME/tools   туда, куда можно писать: системные каталоги не нужны
#   make install                 в ~/.local/bin; PREFIX=… — в другой каталог
#   make vendor                  зависимости в vendor/ — для сборки без сети
#   make offline                 собрать из vendor/, в сеть не обращаясь
#   make rlm                     ещё и модуль RLM
#   make dist                    релиз под все платформы в dist/
#
# Версия — VERSION; её печатают ochan --version и ochan-mcp --version.
# SYSTEM_ROLES — системная политика ролей (docs/guide/security.md): путь
# зашивается в программу, окружением его не поменять.
VERSION      ?= 0.3.0-alpha
PREFIX       ?= $(HOME)/.local
OUT          ?= bin
SYSTEM_ROLES ?= /etc/octochan/roles.yaml

LDFLAGS     := -X octochan/cmd.Version=$(VERSION) -X octochan/rbac.SystemPolicyPath=$(SYSTEM_ROLES)
MCP_LDFLAGS := -X main.serverVersion=$(VERSION)
PLATFORMS   := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build install uninstall rlm vendor offline dist test vet version

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o "$(abspath $(OUT))/ochan" .
	cd mcp && go build -trimpath -ldflags "$(MCP_LDFLAGS)" -o "$(abspath $(OUT))/ochan-mcp" .

install:
	PREFIX="$(PREFIX)" VERSION="$(VERSION)" sh scripts/install.sh

uninstall:
	PREFIX="$(PREFIX)" sh scripts/install.sh --uninstall

rlm:
	PREFIX="$(PREFIX)" VERSION="$(VERSION)" sh scripts/install.sh --rlm

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

test:
	go test -race ./... ./mcp/...

vet:
	go vet ./... && cd mcp && go vet .
