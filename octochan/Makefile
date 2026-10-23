# ochan: make install — и ochan вызывается по имени из любого каталога.
PREFIX ?= $(HOME)/.local

.PHONY: build install uninstall test vet

build:
	go build -trimpath -o bin/ochan .
	cd mcp && go build -trimpath -o ../bin/ochan-mcp .

install:
	PREFIX="$(PREFIX)" sh scripts/install.sh

uninstall:
	PREFIX="$(PREFIX)" sh scripts/install.sh --uninstall

test:
	go test -race ./... ./mcp/...

vet:
	go vet ./... && cd mcp && go vet .
