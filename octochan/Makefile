# ochan: make install — и ochan вызывается по имени из любого каталога.
# make rlm — ещё и модуль RLM (исполнитель сценариев через RLM API).
PREFIX ?= $(HOME)/.local

.PHONY: build install uninstall rlm test vet

build:
	go build -trimpath -o bin/ochan .
	cd mcp && go build -trimpath -o ../bin/ochan-mcp .

install:
	PREFIX="$(PREFIX)" sh scripts/install.sh

uninstall:
	PREFIX="$(PREFIX)" sh scripts/install.sh --uninstall

rlm:
	PREFIX="$(PREFIX)" sh scripts/install.sh --rlm

test:
	go test -race ./... ./mcp/...

vet:
	go vet ./... && cd mcp && go vet .