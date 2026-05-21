.PHONY: build build-gui install install-gui test fmt vet tidy clean dev doctor web web-install

GO       ?= go
BIN      ?= bloodhound
GUI_BIN  ?= bloodhound-gui
LDFLAGS  ?= -X github.com/PeterSR/claude-code-bloodhound/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Build tags for the GUI:
#   prod        — gates //go:embed of web/dist (see web/embed_prod.go)
#   production  — Wails compiles its real Run() implementation; without
#                 this tag wails.Run() is a no-op stub
#   desktop     — selects Wails' desktop frontend (vs. bindings generation)
#   webkit2_41  — link against webkit2gtk-4.1 instead of the default 4.0
#                 (Fedora 43+ and Ubuntu 24.04+ only ship 4.1)
GUI_TAGS ?= prod production desktop webkit2_41

# Where install-gui drops the desktop entry + icons. Default = per-user
# XDG. Override with `make install-gui PREFIX=/usr` for a system install.
PREFIX        ?= $(HOME)/.local
DESKTOP_DIR   ?= $(PREFIX)/share/applications
ICON_BASE_DIR ?= $(PREFIX)/share/icons/hicolor

# `make build` compiles the daemon. No bundle inside — the daemon is the
# JSON API only; the GUI binary carries the React app.
build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/bloodhound

install:
	$(GO) install -ldflags '$(LDFLAGS)' ./cmd/bloodhound
	@dest=$$($(GO) env GOBIN); \
	  if [ -z "$$dest" ]; then dest=$$($(GO) env GOPATH)/bin; fi; \
	  echo "installed: $$dest/$(BIN)"

# `make build-gui` builds the Wails-based native window binary. Linux-only
# for now; requires gtk+-3.0 and webkit2gtk-4.1 dev packages on PATH
# (Fedora: `webkit2gtk4.1-devel`, Ubuntu 24.04+: `libwebkit2gtk-4.1-dev`).
# Depends on `make web` because the bundle is embedded into this binary.
build-gui: web
	$(GO) build -tags '$(GUI_TAGS)' -ldflags '$(LDFLAGS)' -o $(GUI_BIN) ./cmd/bloodhound-gui

# `make install-gui` installs the gui binary, desktop entry, and icons
# into $(PREFIX) (~/.local by default; pass PREFIX=/usr for system).
install-gui: build-gui
	install -d $(DESKTOP_DIR)
	install -m 644 packaging/bloodhound.desktop $(DESKTOP_DIR)/bloodhound.desktop
	install -d $(ICON_BASE_DIR)/512x512/apps
	install -m 644 web/public/icon-512.png $(ICON_BASE_DIR)/512x512/apps/bloodhound.png
	install -d $(ICON_BASE_DIR)/scalable/apps
	install -m 644 web/public/icon.svg $(ICON_BASE_DIR)/scalable/apps/bloodhound.svg
	install -d $(PREFIX)/bin
	install -m 755 $(GUI_BIN) $(PREFIX)/bin/$(GUI_BIN)
	@if command -v update-desktop-database >/dev/null 2>&1; then \
	  update-desktop-database $(DESKTOP_DIR) 2>/dev/null || true; \
	fi
	@if command -v gtk-update-icon-cache >/dev/null 2>&1; then \
	  gtk-update-icon-cache -f -t $(ICON_BASE_DIR) 2>/dev/null || true; \
	fi
	@echo "installed: $(PREFIX)/bin/$(GUI_BIN), $(DESKTOP_DIR)/bloodhound.desktop"

test:
	$(GO) test ./...

fmt:
	gofmt -s -w .

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -f $(BIN) $(GUI_BIN)
	rm -rf web/dist

# `make dev` runs the daemon (API only — the GUI carries the UI now).
# For UI dev, `cd web && npm run dev` and Vite proxies /api to the
# daemon's unix socket ($XDG_RUNTIME_DIR/bloodhound/api.sock).
dev:
	$(GO) run ./cmd/bloodhound daemon

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
