.PHONY: build install test fmt vet tidy clean dev doctor web web-install

GO       ?= go
BIN      ?= bloodhound
LDFLAGS  ?= -X github.com/PeterSR/claude-code-bloodhound/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
TAGS     ?= prod

# `make build` is the canonical, production-shaped build: ensure the React
# bundle is present, then compile the Go binary with the `prod` build tag so
# //go:embed picks it up.
build: web
	$(GO) build -tags '$(TAGS)' -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/bloodhound

install: web
	$(GO) install -tags '$(TAGS)' -ldflags '$(LDFLAGS)' ./cmd/bloodhound
	@dest=$$($(GO) env GOBIN); \
	  if [ -z "$$dest" ]; then dest=$$($(GO) env GOPATH)/bin; fi; \
	  echo "installed: $$dest/$(BIN)"

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

# `make dev` runs the Go server without the prod tag, so the placeholder
# page is served. Run `cd web && npm run dev` separately for the full
# UI in dev (Vite proxies /api -> :7777).
dev:
	$(GO) run ./cmd/bloodhound serve

doctor:
	$(GO) run ./cmd/bloodhound doctor

# Build the React bundle. Idempotent: re-runs npm install only if the
# lockfile is newer than node_modules.
web: web/node_modules
	cd web && npm run build

web/node_modules: web/package-lock.json
	cd web && npm install
	@touch web/node_modules

web-install: web/node_modules
