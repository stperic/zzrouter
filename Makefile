.PHONY: all help build build-client build-server build-launcher build-cross release-preflight bc bw test test-coverage test-stress clean run dev fmt vet lint fix install uninstall verify deps-dev release version-info winres winres-clean

# Load .env file if it exists (provides DEPLOY_HOST, HF_TOKEN, etc.)
-include .env
export

# Platform for cross-compilation (default: current platform)
PLATFORM ?= $(shell go env GOOS)-$(shell go env GOARCH)

# Host executable extension (.exe on Windows, empty elsewhere). Needed because
# `go build -o zzrouter` writes the file literally as `zzrouter` even on
# Windows -- the -o flag overrides Go's auto-append of `.exe`. install.ps1
# looks for `.exe`, so local build targets must emit matching filenames.
GOEXE ?= $(shell go env GOEXE)

# Deploy host for bw target (e.g., DEPLOY_HOST=192.0.2.10)
DEPLOY_HOST ?=

# Version information : git is the single source of truth. The nearest tag
# (e.g. `v0.1.1` or `v0.1.1-rc2`) is split into Major/Minor/Patch/PreRelease
# and injected into pkg/version via -ldflags. If git is unavailable the Go
# defaults (empty → "0.0.0-dev") make it obvious this is an unstamped build.
COMMIT       ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE   ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# `git describe --tags --abbrev=0` walks commit ancestry and fails if no tag
# is reachable from HEAD (e.g. the tag was placed on a branch that hasn't been
# merged). Fall back to the newest version-sorted `v*` tag so release builds
# still stamp a sensible version instead of 0.0.0-dev.
VERSION_TAG  := $(shell git describe --tags --abbrev=0 2>/dev/null || git tag -l --sort=-v:refname 'v*' | head -n1)
VERSION_BARE := $(patsubst v%,%,$(VERSION_TAG))
VERSION_CORE := $(firstword $(subst -, ,$(VERSION_BARE)))
VERSION_PRE  := $(word 2,$(subst -, ,$(VERSION_BARE)))
MAJOR        := $(word 1,$(subst ., ,$(VERSION_CORE)))
MINOR        := $(word 2,$(subst ., ,$(VERSION_CORE)))
PATCH        := $(word 3,$(subst ., ,$(VERSION_CORE)))

# Embedded version string, matching what the binary will report at runtime
# (SemVer 2.0 : build metadata is added by pkg/version/init() from BUILD_DATE).
EMBEDDED     := $(MAJOR).$(MINOR).$(PATCH)$(if $(VERSION_PRE),-$(VERSION_PRE))

# Build flags
LDFLAGS := -s -w \
	-X github.com/stperic/zzrouter/pkg/version.Major=$(MAJOR) \
	-X github.com/stperic/zzrouter/pkg/version.Minor=$(MINOR) \
	-X github.com/stperic/zzrouter/pkg/version.Patch=$(PATCH) \
	-X github.com/stperic/zzrouter/pkg/version.PreRelease=$(VERSION_PRE) \
	-X github.com/stperic/zzrouter/pkg/version.GitCommit=$(COMMIT) \
	-X github.com/stperic/zzrouter/pkg/version.BuildDate=$(BUILD_DATE)

# CGO flags to suppress macOS deprecation warnings
export CGO_CFLAGS=-Wno-deprecated-declarations

# Windows version: Major.Minor.Patch.0 (four-part, required by PE VERSIONINFO)
WIN_VERSION := $(MAJOR).$(MINOR).$(PATCH).0

# Detect target OS for conditional Windows resource embedding.
# GOOS may be set for cross-compilation; default to the host OS.
TARGET_OS ?= $(shell go env GOOS)

# Default target
all: build

# Build client, launcher, then server (server embeds launcher hash for integrity verification)
build: build-client build-launcher build-server

# --- Windows resource embedding (icon, manifest, version info) ---------------
# go-winres generates .syso files that `go build` links automatically. The JSON
# configs live in winres/ (one per binary); .syso files are build artifacts and
# should NOT be committed. Install go-winres: go install github.com/tc-hib/go-winres@latest
WINRES_DIR := winres
WINRES_BINS := zzrouter zzrouter-node zzrouter-launcher
SYSO_FILES := $(foreach bin,$(WINRES_BINS),cmd/$(bin)/rsrc_windows_amd64.syso)

winres: $(SYSO_FILES)

cmd/%/rsrc_windows_amd64.syso: $(WINRES_DIR)/%.json $(WINRES_DIR)/icon.png $(WINRES_DIR)/icon16.png
	@command -v go-winres >/dev/null 2>&1 || { \
		echo "ERROR: go-winres not found. Install the dev tooling with 'make deps-dev'." >&2; \
		exit 1; }
	@echo "Generating Windows resources for $*..."
	@go-winres make --in $(WINRES_DIR)/$*.json --out cmd/$*/rsrc --arch amd64 \
		--product-version $(WIN_VERSION) --file-version $(WIN_VERSION)

winres-clean:
	@rm -f $(SYSO_FILES)

# Build the client binary
build-client: $(if $(filter windows,$(TARGET_OS)),cmd/zzrouter/rsrc_windows_amd64.syso)
	@echo "Building zzrouter$(GOEXE)..."
	@go build -ldflags "$(LDFLAGS)" -o zzrouter$(GOEXE) ./cmd/zzrouter
	@echo "Build complete: zzrouter$(GOEXE)"

# Build the launcher binary
build-launcher: $(if $(filter windows,$(TARGET_OS)),cmd/zzrouter-launcher/rsrc_windows_amd64.syso)
	@echo "Building zzrouter-launcher$(GOEXE)..."
	@go build -ldflags "$(LDFLAGS)" -o zzrouter-launcher$(GOEXE) ./cmd/zzrouter-launcher
	@echo "Build complete: zzrouter-launcher$(GOEXE)"

# Build the server binary (embeds launcher SHA256 for integrity verification).
# The -tags release flag activates strict integrity mode; combined with the
# non-empty hash assertion below, this makes it impossible to ship a release
# binary that silently skips launcher verification.
build-server: $(if $(filter windows,$(TARGET_OS)),cmd/zzrouter-node/rsrc_windows_amd64.syso)
	@echo "Building zzrouter-node$(GOEXE)..."
	@if [ ! -f zzrouter-launcher$(GOEXE) ]; then echo "ERROR: zzrouter-launcher$(GOEXE) not found : run 'make build-launcher' first" >&2; exit 1; fi
	@launcher_sha=$$(shasum -a 256 zzrouter-launcher$(GOEXE) | cut -d' ' -f1); \
	if [ -z "$$launcher_sha" ]; then echo "ERROR: launcher SHA256 is empty : refusing to build release server without integrity hash" >&2; exit 1; fi; \
	go build -tags release -ldflags "$(LDFLAGS) -X github.com/stperic/zzrouter/pkg/prov_apps/process.LauncherSHA256=$$launcher_sha" -o zzrouter-node$(GOEXE) ./cmd/zzrouter-node
	@echo "Build complete: zzrouter-node$(GOEXE) (release, integrity-verified)"

# Build for specific platform (usage: make build-cross PLATFORM=linux-amd64).
# This is the server binary alone, for a quick cross-compile check. It is
# NOT enough to install a node: that wants all three binaries with the
# launcher SHA embedded, which is `make release RELEASE_PLATFORMS=<platform>`.
build-cross:
	@echo "Building for platform: $(PLATFORM)"
	$(eval GOOS := $(word 1, $(subst -, ,$(PLATFORM))))
	$(eval GOARCH := $(word 2, $(subst -, ,$(PLATFORM))))
	$(eval CROSS_EXT := $(if $(filter windows,$(GOOS)),.exe,))
	@echo "GOOS=$(GOOS), GOARCH=$(GOARCH)"
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -o zzrouter-node-$(PLATFORM)$(CROSS_EXT) ./cmd/zzrouter-node
	@echo "Built: zzrouter-node-$(PLATFORM)$(CROSS_EXT) (server only)"
	@echo "For a full node install, use: make release RELEASE_PLATFORMS=$(PLATFORM)"

# Default install directory for local symlinks. Override when ~/.local/bin
# is not on your PATH (e.g. `INSTALL_DIR=/usr/local/bin sudo make install`).
INSTALL_DIR ?= $(HOME)/.local/bin

# install deploys the three binaries by SYMLINKING $(INSTALL_DIR) entries to
# the repo build artifacts. Symlinks (not copies) guarantee the on-PATH
# binaries cannot drift from the repo build : which matters because the
# server binary embeds the launcher's SHA256 at build time and SIGKILLs
# itself at startup if the on-PATH launcher doesn't match. One build, one
# physical binary set, no drift, no partial-deploy footgun.
install: build
	@mkdir -p $(INSTALL_DIR)
	@for bin in zzrouter zzrouter-launcher zzrouter-node; do \
		rm -f $(INSTALL_DIR)/$$bin$(GOEXE); \
		ln -s $(CURDIR)/$$bin$(GOEXE) $(INSTALL_DIR)/$$bin$(GOEXE); \
	done
	@echo "Installed symlinks in $(INSTALL_DIR):"
	@ls -l $(INSTALL_DIR)/zzrouter$(GOEXE) $(INSTALL_DIR)/zzrouter-launcher$(GOEXE) $(INSTALL_DIR)/zzrouter-node$(GOEXE)

# uninstall removes the symlinks from $(INSTALL_DIR). Repo build artifacts
# are left in place; use `make clean` to remove those.
uninstall:
	@for bin in zzrouter zzrouter-launcher zzrouter-node; do \
		rm -f $(INSTALL_DIR)/$$bin$(GOEXE); \
	done
	@echo "Removed symlinks from $(INSTALL_DIR)"

# Build local coordinator and restart
bc: build
	@echo "Restarting local coordinator..."
	@./zzrouter-node$(GOEXE) stop 2>/dev/null || true
	@./zzrouter-node$(GOEXE) start

# Build Linux AMD64 binaries and deploy to worker
# Usage: make bw DEPLOY_HOST=192.0.2.10
bw:
	@echo "Building Linux AMD64 binaries for deployment..."
	@echo "Building zzrouter-launcher for linux/amd64..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o zzrouter-launcher-linux-amd64 ./cmd/zzrouter-launcher
	@echo "Building zzrouter-node for linux/amd64..."
	@launcher_sha=$$(shasum -a 256 zzrouter-launcher-linux-amd64 | cut -d' ' -f1); \
	if [ -z "$$launcher_sha" ]; then echo "ERROR: launcher SHA256 is empty : refusing to build release server without integrity hash" >&2; exit 1; fi; \
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags release -ldflags "$(LDFLAGS) -X github.com/stperic/zzrouter/pkg/prov_apps/process.LauncherSHA256=$$launcher_sha" -o zzrouter-node-linux-amd64 ./cmd/zzrouter-node
	@echo "Building zzrouter client for linux/amd64..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o zzrouter-linux-amd64 ./cmd/zzrouter
	@echo "Linux AMD64 builds complete"
ifdef DEPLOY_HOST
	@echo "Stopping remote server..."
	-ssh root@$(DEPLOY_HOST) "systemctl stop zzrouter-node || pkill -f zzrouter-node || true"
	@echo "Ensuring remote directory exists..."
	ssh root@$(DEPLOY_HOST) "mkdir -p /opt/zzrouter"
	@echo "Backing up existing binaries..."
	-ssh root@$(DEPLOY_HOST) "if [ -f /opt/zzrouter/zzrouter-node ]; then mv /opt/zzrouter/zzrouter-node /opt/zzrouter/zzrouter-node.bak; fi"
	-ssh root@$(DEPLOY_HOST) "if [ -f /opt/zzrouter/zzrouter ]; then mv /opt/zzrouter/zzrouter /opt/zzrouter/zzrouter.bak; fi"
	-ssh root@$(DEPLOY_HOST) "if [ -f /opt/zzrouter/zzrouter-launcher ]; then mv /opt/zzrouter/zzrouter-launcher /opt/zzrouter/zzrouter-launcher.bak; fi"
	@echo "Deploying to remote server ($(DEPLOY_HOST))..."
	scp ./zzrouter-node-linux-amd64 root@$(DEPLOY_HOST):/opt/zzrouter/zzrouter-node
	scp ./zzrouter-launcher-linux-amd64 root@$(DEPLOY_HOST):/opt/zzrouter/zzrouter-launcher
	scp ./zzrouter-linux-amd64 root@$(DEPLOY_HOST):/opt/zzrouter/zzrouter
	@echo "Setting executable permissions..."
	ssh root@$(DEPLOY_HOST) "chmod +x /opt/zzrouter/zzrouter-node /opt/zzrouter/zzrouter-launcher /opt/zzrouter/zzrouter"
	@echo "Creating symlinks in /usr/local/bin/..."
	ssh root@$(DEPLOY_HOST) "ln -sf /opt/zzrouter/zzrouter-node /usr/local/bin/zzrouter-node && ln -sf /opt/zzrouter/zzrouter /usr/local/bin/zzrouter && ln -sf /opt/zzrouter/zzrouter-launcher /usr/local/bin/zzrouter-launcher"
	@echo "Installing and starting service..."
	ssh root@$(DEPLOY_HOST) "/opt/zzrouter/zzrouter-node start"
	@echo "Deployment complete on $(DEPLOY_HOST)"
else
	@echo "Skipping deploy: set DEPLOY_HOST to deploy (e.g., make bw DEPLOY_HOST=192.0.2.10)"
endif

# Run tests
test:
	go test -v -race -timeout 15m ./...

# Run tests with coverage
test-coverage:
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Run tests repeatedly under -race to surface flakes before they hit CI.
# Pre-push smoke for timing-sensitive suites (mesh, cluster, model.cache).
# Count is intentionally lower than the full suite : 20 iterations is
# enough to catch the class of flakes we've actually shipped regressions
# for (download-tracker debounce, pairing-join races) without a 20-minute
# wall wait. Override with TEST_STRESS_COUNT for a deeper sweep.
#
# Note: -timeout is per-package, not aggregate (go test semantics). 30m
# is generous for any single package at count=20; a full ./... run
# wall-clocks in the several-minutes range on a dev laptop.
TEST_STRESS_COUNT ?= 20
test-stress:
	go test -race -count=$(TEST_STRESS_COUNT) -timeout 30m ./...

# Clean build artifacts
clean: winres-clean
	rm -f zzrouter zzrouter.exe zzrouter-node zzrouter-node.exe zzrouter-launcher zzrouter-launcher.exe zzrouter-node-* zzrouter-launcher-* zzrouter-linux-* coverage.out coverage.html
	go clean

# Run the server with default config
run: build-server
	./zzrouter-node$(GOEXE) start

# Run in development mode
dev: build-server
	ZZROUTER_DEBUG=true ./zzrouter-node$(GOEXE) start

# Format code
fmt:
	go fmt ./...

# Modernize code (apply Go 1.26+ idioms: any, slices.Contains, min/max, etc.)
fix:
	go fix ./...

# Lint code
lint:
	golangci-lint run --timeout 5m

# Run go vet
vet:
	go vet ./...

# Install development dependencies
deps-dev:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4
	go install github.com/tc-hib/go-winres@latest

# Verify dependencies
verify:
	go mod verify
	go mod tidy

## Release platforms (GOOS-GOARCH). Each gets its own dist/<platform>/ folder
## with zzrouter, zzrouter-launcher and zzrouter-node (server embeds the
## launcher's SHA256 for integrity verification).
RELEASE_PLATFORMS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64

# release-preflight clears dist BEFORE anything can fail. A release that
# dies partway (a missing go-winres, a compile error) used to leave the
# previous build sitting in dist/, which then reads as a successful build
# of the current tree: the artifacts are there, dated whenever they were
# last built. Nothing downstream can tell the difference.
release-preflight:
	@rm -rf dist

# Create release builds: all three binaries for every platform in
# RELEASE_PLATFORMS, plus that platform's installer.
#
# The installer ships IN the bundle deliberately. Shipping binaries alone
# means the operator fetches the install script from somewhere else, with
# nothing tying the two versions together : so a bundle can be installed
# by a script that predates it, which is how a fixed installer gets used
# in its broken form.
release: release-preflight winres
	@echo "Building release binaries for: $(RELEASE_PLATFORMS)"
	@mkdir -p dist
	@bash .github/scripts/third-party-notices.sh dist/THIRD_PARTY_NOTICES
	@for platform in $(RELEASE_PLATFORMS); do \
		goos=$${platform%-*}; \
		goarch=$${platform#*-}; \
		ext=""; \
		if [ "$$goos" = "windows" ]; then ext=".exe"; fi; \
		outdir="dist/$$platform"; \
		mkdir -p "$$outdir"; \
		echo "==> $$platform"; \
		echo "   building zzrouter-launcher"; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -ldflags "$(LDFLAGS)" -o "$$outdir/zzrouter-launcher$$ext" ./cmd/zzrouter-launcher || exit 1; \
		launcher_sha=$$(shasum -a 256 "$$outdir/zzrouter-launcher$$ext" | cut -d' ' -f1); \
		if [ -z "$$launcher_sha" ]; then echo "ERROR: launcher sha empty for $$platform" >&2; exit 1; fi; \
		echo "   building zzrouter-node (release, launcher sha=$$launcher_sha)"; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -tags release -ldflags "$(LDFLAGS) -X github.com/stperic/zzrouter/pkg/prov_apps/process.LauncherSHA256=$$launcher_sha" -o "$$outdir/zzrouter-node$$ext" ./cmd/zzrouter-node || exit 1; \
		echo "   building zzrouter (client)"; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -ldflags "$(LDFLAGS)" -o "$$outdir/zzrouter$$ext" ./cmd/zzrouter || exit 1; \
		echo "   bundling installer and notices"; \
		cp LICENSE NOTICE dist/THIRD_PARTY_NOTICES "$$outdir/" || exit 1; \
		if [ "$$goos" = "windows" ]; then \
			cp scripts/install.ps1 scripts/clean.ps1 "$$outdir/" || exit 1; \
		else \
			cp scripts/install.sh "$$outdir/" || exit 1; \
			chmod +x "$$outdir/install.sh"; \
		fi; \
	done
	@rm dist/THIRD_PARTY_NOTICES
	@$(MAKE) winres-clean
	@echo "Creating checksums..."
	@cd dist && find . -type f ! -name 'checksums.txt' -print0 | xargs -0 shasum -a 256 > checksums.txt
	@echo "Release builds complete in dist/"

# Show version information that will be embedded in the binary
version-info:
	@echo "Version information:"
	@echo "  Tag:        $(VERSION_TAG)"
	@echo "  Embedded:   $(EMBEDDED)"
	@echo "  Commit:     $(COMMIT)"
	@echo "  Build Date: $(BUILD_DATE)"
	@echo ""
	@echo "LDFLAGS: $(LDFLAGS)"

# Show help
help:
	@echo "zzRouter - Distributed Large Language Model Management"
	@echo ""
	@echo "Available commands:"
	@echo "  make build            - Build the server binary"
	@echo "  make build-cross      - Build for specific platform (PLATFORM=linux-amd64)"
	@echo "  make install          - Build and symlink binaries into \$$INSTALL_DIR (default ~/.local/bin)"
	@echo "  make uninstall        - Remove symlinks from \$$INSTALL_DIR"
	@echo "  make bc               - Build and restart local coordinator"
	@echo "  make bw               - Build Linux AMD64; set DEPLOY_HOST for deployment"
	@echo "  make test             - Run tests"
	@echo "  make test-coverage    - Run tests with coverage report"
	@echo "  make clean            - Clean build artifacts"
	@echo "  make run              - Build and run the server"
	@echo "  make dev              - Run in development mode with debug logging"
	@echo "  make fmt              - Format code"
	@echo "  make lint             - Run golangci-lint"
	@echo "  make vet              - Run go vet"
	@echo "  make deps-dev         - Install development dependencies"
	@echo "  make verify           - Verify and tidy dependencies"
	@echo "  make release          - Create release builds for all platforms"
	@echo "  make version-info     - Show version information"
	@echo "  make help             - Show this help"
	@echo ""
	@echo "Supported platforms for build-cross:"
	@echo "  linux-amd64, linux-arm64, darwin-amd64, darwin-arm64, windows-amd64"
	@echo ""
	@echo "Examples:"
	@echo "  make build"
	@echo "  make build-cross PLATFORM=linux-amd64"
	@echo "  make test"
	@echo "  make release"
