.PHONY: build test fmt vet tidy clean dev doctor web web-install

GO        ?= go
BIN       ?= bloodhound
LDFLAGS   ?= -X github.com/PeterSR/claude-code-bloodhound/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/bloodhound

test:
	$(GO) test ./...

fmt:
	gofmt -s -w .

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -f $(BIN)
	rm -rf web/dist

dev:
	$(GO) run ./cmd/bloodhound serve

doctor:
	$(GO) run ./cmd/bloodhound doctor

web-install:
	cd web && npm install

web:
	cd web && npm run build
