# sion-backup — keeps a work computer backed up, and says so.
#
# The fleet is Windows, macOS and Linux, and nothing here needs a C toolchain:
# the SQLite driver is pure Go and the platform keyrings are reached through
# DPAPI, /usr/bin/security and secret-tool rather than through cgo. So one
# machine builds for all three, and `make release-all` is the proof.

GO      ?= go
BINARY  ?= sion-backup
CMD     ?= ./cmd/sion-backup
ADDR    ?= 127.0.0.1:7391
DIST    ?= dist

# Stamped into the binary and reported to the fleet dashboard, which is how
# "which machines are still on the old build" becomes answerable without
# visiting them.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -X main.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets, grouped
	@awk ' \
		/^##@ / { sec = substr($$0, 5); if (!(sec in seen)) { seen[sec] = 1; order[++n] = sec }; next } \
		/^[a-zA-Z0-9_-]+:.*## / { \
			name = $$0; sub(/:.*/, "", name); \
			desc = $$0; sub(/^[^#]*## /, "", desc); \
			items[sec] = items[sec] sprintf("  %-16s %s\n", name, desc) \
		} \
		END { for (i = 1; i <= n; i++) printf "\n%s\n%s", order[i], items[order[i]] } \
	' $(MAKEFILE_LIST)

##@ Development

.PHONY: deps
deps: ## Download module dependencies into the module cache
	$(GO) mod download

.PHONY: build
build: ## Build the binary for this machine
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINARY) $(CMD)

.PHONY: run
run: build ## Run the daemon here (make run ADDR=127.0.0.1:9090)
	./$(BINARY) daemon -addr $(ADDR) -v

.PHONY: doctor
doctor: build ## Check this machine's setup
	./$(BINARY) doctor

.PHONY: fmt
fmt: ## Format
	$(GO) fmt ./...

.PHONY: vet
vet: ## Vet
	$(GO) vet ./...

.PHONY: test
test: ## Run the tests
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run the tests with the race detector
	@# Worth its own target and worth running: the daemon reads the run
	@# progress from the HTTP handler while restic's output is being parsed in
	@# another goroutine, which is exactly the shape of bug this catches.
	$(GO) test -race ./...

.PHONY: cover
cover: ## Run the tests and open a coverage report
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out

.PHONY: restic
restic: ## Download the pinned restic into dist/, for the real-restic tests
	@# For CI and a working copy. A deployed machine does not need this:
	@# sion-backup installs its own restic from the pin compiled into it, and
	@# "sion-backup restic" is the command for that.
	@#
	@# The version and its hashes live in deploy/restic.pin, copied from a
	@# signed upstream SHA256SUMS and kept in step with foundation/restic/pin.go
	@# by TestPinMatchesDeployFile. The script refuses anything that does not
	@# match and leaves nothing behind when it does.
	./scripts/fetch-restic "$(GOOS)" "$(GOARCH)" $(DIST)

.PHONY: api-check
api-check: ## Validate docs/openapi.yaml and every example in it
	@# A venv rather than the system Python: this is the only Python in the
	@# project and it should not be able to break anything outside itself.
	@test -d .venv-api || python3 -m venv .venv-api
	@.venv-api/bin/pip install -q -r scripts/requirements-api.txt
	@.venv-api/bin/python scripts/check-api-spec.py

.PHONY: api-docs
api-docs: ## Render docs/openapi.yaml into a readable docs/api.html
	@test -d .venv-api || python3 -m venv .venv-api
	@.venv-api/bin/pip install -q -r scripts/requirements-api.txt
	@.venv-api/bin/python scripts/render-api-docs.py
	@echo "open docs/api.html in a browser"

.PHONY: check
check: fmt vet test cross ## Everything CI runs (except api-check, which needs Python)

##@ Cross-platform

.PHONY: cross
cross: ## Type-check for all three platforms without producing binaries
	@# The build tags in foundation/secrets mean a change can compile here and
	@# break on Windows. This is one command and it catches that.
	@for os in linux darwin windows; do \
		echo "  $$os"; \
		GOOS=$$os GOARCH=amd64 $(GO) build -o /dev/null ./... || exit 1; \
	done

.PHONY: release
release: ## Build a stripped static binary (make release GOOS=windows GOARCH=amd64)
	@# CGO_ENABLED=0 is the point of this target, not a detail. An ordinary
	@# build links against the host's libc, and a binary built that way on one
	@# machine fails on another with "GLIBC_2.xx not found" — a thoroughly
	@# confusing thing to meet on somebody else's laptop.
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -trimpath -ldflags '-s -w $(LDFLAGS)' \
		-o $(DIST)/$(BINARY)-$(GOOS)-$(GOARCH)$(if $(filter windows,$(GOOS)),.exe,) $(CMD)

.PHONY: release-all
release-all: ## Build every binary the fleet needs
	@mkdir -p $(DIST)
	$(MAKE) release GOOS=windows GOARCH=amd64
	$(MAKE) release GOOS=darwin  GOARCH=amd64
	$(MAKE) release GOOS=darwin  GOARCH=arm64
	$(MAKE) release GOOS=linux   GOARCH=amd64
	$(MAKE) release GOOS=linux   GOARCH=arm64
	@echo
	@ls -lh $(DIST)

.PHONY: checksums
checksums: ## Write SHA-256 sums for the release binaries
	@# The installer verifies these before running anything. A binary fetched
	@# over a network is a binary that will hold every credential on the
	@# machine within a minute of starting.
	cd $(DIST) && sha256sum $(BINARY)-* > SHA256SUMS
	@cat $(DIST)/SHA256SUMS

##@ Releasing

# The three of these do the same thing to different parts of the version.
# The rules that stop a release going out from the wrong commit live in
# scripts/tag, which explains why each one is there.

.PHONY: tag-revision
tag-revision: ## Tag the next patch release, vX.Y.(Z+1), and push it
	@scripts/tag revision

.PHONY: tag-minor
tag-minor: ## Tag the next minor release, vX.(Y+1).0, and push it
	@scripts/tag minor

.PHONY: tag-major
tag-major: ## Tag the next major release, v(X+1).0.0, and push it
	@scripts/tag major

##@ Housekeeping

.PHONY: tidy
tidy: ## Tidy go.mod
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BINARY) $(DIST) coverage.out .venv-api

.PHONY: paths
paths: build ## Print where this program keeps its files on this machine
	./$(BINARY) paths
