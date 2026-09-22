# kindling-sandbox — la extensión de kling que sirve sandboxes de agentes de
# código a través de un frontal.
#
#   make install                       kling-sandbox en tu máquina
#   make deploy HOST=ssh://juan@lab    el frontal en el host del daemon
#
# Necesita kindling >= v0.7 instalado: esto no hace nada por su cuenta, añade
# `kling sbx` y habla con uno o varios daemons de kindling por su API.

BIN     := kling-sandbox
PKG     := ./cmd/kling-sandbox
GOARCH  ?= amd64
PREFIX  ?= $(shell for d in "$$HOME/.local" "$$HOME/go" /opt/homebrew /usr/local; do \
             [ -w "$$d/bin" ] && echo "$$d" && exit; done; echo "$$HOME/.local")
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
HOST ?= $(shell kling config show 2>/dev/null | awk '/^contexto:|^context:/{print $$2}')

.PHONY: all build install uninstall linux deploy test fmt clean

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## install — kling descubre la extensión por estar en el PATH con este nombre
install: build
	@mkdir -p $(PREFIX)/bin 2>/dev/null || sudo mkdir -p $(PREFIX)/bin
	@install -m755 $(BIN) $(PREFIX)/bin/$(BIN) 2>/dev/null \
		|| sudo install -m755 $(BIN) $(PREFIX)/bin/$(BIN)
	@echo "instalado: $(PREFIX)/bin/$(BIN)  ($(VERSION))"
	@command -v kling >/dev/null 2>&1 || echo "AVISO: no encuentro kling; instala kindling primero."
	@echo
	@echo "Comprueba que kling la ve:  kling plugins"

uninstall:
	@rm -f $(PREFIX)/bin/$(BIN) 2>/dev/null || sudo rm -f $(PREFIX)/bin/$(BIN)

linux:
	GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-$(GOARCH) $(PKG)

## deploy — el frontal en el host del daemon, con su unidad y su token
deploy: linux
	@test -n "$(HOST)" || { echo "usa: make deploy HOST=ssh://usuario@maquina" >&2; exit 1; }
	$(eval TARGET := $(patsubst ssh://%,%,$(HOST)))
	scp -q $(BIN)-linux-$(GOARCH) $(TARGET):/tmp/$(BIN)
	scp -q packaging/kling-sandbox.service $(TARGET):/tmp/
	ssh $(TARGET) 'test -x /usr/local/bin/kling || { echo "falta kling en el host: despliega kindling primero" >&2; exit 1; } && \
		sudo install -m755 /tmp/$(BIN) /usr/local/bin/$(BIN) && \
		sudo install -m644 /tmp/kling-sandbox.service /etc/systemd/system/ && \
		sudo install -d -m755 /etc/kling && \
		( [ -s /etc/kling/sandbox.env ] || \
		  ( sudo install -m600 /dev/null /etc/kling/sandbox.env && \
		  printf "KLING_SANDBOX_TOKEN=%s\n" \
		    "$$(head -c32 /dev/urandom | base64 | tr "+/" "\-_" | tr -d "=")" \
		  | sudo tee /etc/kling/sandbox.env >/dev/null ) ) && \
		sudo chmod 600 /etc/kling/sandbox.env && \
		sudo systemctl daemon-reload && sudo systemctl enable kling-sandbox && \
		sudo systemctl restart kling-sandbox && sleep 1 && systemctl is-active kling-sandbox'
	@echo "frontal desplegado en $(TARGET)"
	@echo
	@echo "Apunta tu CLI al token:"
	@echo "  kling config set sandbox.token \\"
	@echo "    \$$(ssh $(TARGET) 'sudo cut -d= -f2 /etc/kling/sandbox.env')"

test:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	go test -race ./...

fmt:
	gofmt -l -w .

clean:
	rm -f $(BIN) $(BIN)-linux-amd64 $(BIN)-linux-arm64
