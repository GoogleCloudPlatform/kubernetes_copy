# Copyright The OpenTelemetry Authors
# SPDX-License-Identifier: Apache-2.0

TOOLS_MOD_DIR := ./internal/tools

ALL_DOCS := $(shell find . -name '*.md' -type f | sort)
ALL_GO_MOD_DIRS := $(shell find . -type f -name 'go.mod' -exec dirname {} \; | sort)
OTEL_GO_MOD_DIRS := $(filter-out $(TOOLS_MOD_DIR), $(ALL_GO_MOD_DIRS))
ALL_COVERAGE_MOD_DIRS := $(shell find . -type f -name 'go.mod' -exec dirname {} \; | grep -E -v '^./example|^$(TOOLS_MOD_DIR)' | sort)

GO = go
TIMEOUT = 60

.DEFAULT_GOAL := precommit

.PHONY: precommit ci
precommit: generate toolchain-check license-check misspell go-mod-tidy golangci-lint-fix verify-readmes verify-mods test-default
ci: generate toolchain-check license-check lint vanity-import-check verify-readmes verify-mods build test-default check-clean-work-tree test-coverage

# Tools

TOOLS = $(CURDIR)/.tools

$(TOOLS):
	@mkdir -p $@
$(TOOLS)/%: $(TOOLS_MOD_DIR)/go.mod | $(TOOLS)
	cd $(TOOLS_MOD_DIR) && \
	$(GO) build -o $@ $(PACKAGE)

MULTIMOD = $(TOOLS)/multimod
$(TOOLS)/multimod: PACKAGE=go.opentelemetry.io/build-tools/multimod

SEMCONVGEN = $(TOOLS)/semconvgen
$(TOOLS)/semconvgen: PACKAGE=go.opentelemetry.io/build-tools/semconvgen

CROSSLINK = $(TOOLS)/crosslink
$(TOOLS)/crosslink: PACKAGE=go.opentelemetry.io/build-tools/crosslink

SEMCONVKIT = $(TOOLS)/semconvkit
$(TOOLS)/semconvkit: PACKAGE=go.opentelemetry.io/otel/$(TOOLS_MOD_DIR)/semconvkit

GOLANGCI_LINT = $(TOOLS)/golangci-lint
$(TOOLS)/golangci-lint: PACKAGE=github.com/golangci/golangci-lint/cmd/golangci-lint

MISSPELL = $(TOOLS)/misspell
$(TOOLS)/misspell: PACKAGE=github.com/client9/misspell/cmd/misspell

GOCOVMERGE = $(TOOLS)/gocovmerge
$(TOOLS)/gocovmerge: PACKAGE=github.com/wadey/gocovmerge

STRINGER = $(TOOLS)/stringer
$(TOOLS)/stringer: PACKAGE=golang.org/x/tools/cmd/stringer

PORTO = $(TOOLS)/porto
$(TOOLS)/porto: PACKAGE=github.com/jcchavezs/porto/cmd/porto

GOTMPL = $(TOOLS)/gotmpl
$(GOTMPL): PACKAGE=go.opentelemetry.io/build-tools/gotmpl

GORELEASE = $(TOOLS)/gorelease
$(GORELEASE): PACKAGE=golang.org/x/exp/cmd/gorelease

GOVULNCHECK = $(TOOLS)/govulncheck
$(TOOLS)/govulncheck: PACKAGE=golang.org/x/vuln/cmd/govulncheck

.PHONY: tools
tools: $(CROSSLINK) $(GOLANGCI_LINT) $(MISSPELL) $(GOCOVMERGE) $(STRINGER) $(PORTO) $(SEMCONVGEN) $(MULTIMOD) $(SEMCONVKIT) $(GOTMPL) $(GORELEASE)

# Virtualized python tools via docker

# The directory where the virtual environment is created.
VENVDIR := venv

# The directory where the python tools are installed.
PYTOOLS := $(VENVDIR)/bin

# The pip executable in the virtual environment.
PIP := $(PYTOOLS)/pip

# The directory in the docker image where the current directory is mounted.
WORKDIR := /workdir

# The python image to use for the virtual environment.
PYTHONIMAGE := python:3.11.3-slim-bullseye

# Run the python image with the current directory mounted.
DOCKERPY := docker run --rm -v "$(CURDIR):$(WORKDIR)" -w $(WORKDIR) $(PYTHONIMAGE)

# Create a virtual environment for Python tools.
$(PYTOOLS):
# The `--upgrade` flag is needed to ensure that the virtual environment is
# created with the latest pip version.
	@$(DOCKERPY) bash -c "python3 -m venv $(VENVDIR) && $(PIP) install --upgrade pip"

# Install python packages into the virtual environment.
$(PYTOOLS)/%: $(PYTOOLS)
	@$(DOCKERPY) $(PIP) install -r requirements.txt

CODESPELL = $(PYTOOLS)/codespell
$(CODESPELL): PACKAGE=codespell

# Generate

.PHONY: generate
generate: go-generate vanity-import-fix

.PHONY: go-generate
go-generate: $(OTEL_GO_MOD_DIRS:%=go-generate/%)
go-generate/%: DIR=$*
go-generate/%: $(STRINGER) $(GOTMPL)
	@echo "$(GO) generate $(DIR)/..." \
		&& cd $(DIR) \
		&& PATH="$(TOOLS):$${PATH}" $(GO) generate ./...

.PHONY: vanity-import-fix
vanity-import-fix: $(PORTO)
	@$(PORTO) --include-internal -w .

# Generate go.work file for local development.
.PHONY: go-work
go-work: $(CROSSLINK)
	$(CROSSLINK) work --root=$(shell pwd)

# Build

.PHONY: build

build: $(OTEL_GO_MOD_DIRS:%=build/%) $(OTEL_GO_MOD_DIRS:%=build-tests/%)
build/%: DIR=$*
build/%:
	@echo "$(GO) build $(DIR)/..." \
		&& cd $(DIR) \
		&& $(GO) build ./...

build-tests/%: DIR=$*
build-tests/%:
	@echo "$(GO) build tests $(DIR)/..." \
		&& cd $(DIR) \
		&& $(GO) list ./... \
		| grep -v third_party \
		| xargs $(GO) test -vet=off -run xxxxxMatchNothingxxxxx >/dev/null

# Tests

TEST_TARGETS := test-default test-bench test-short test-verbose test-race test-concurrent-safe
.PHONY: $(TEST_TARGETS) test
test-default test-race: ARGS=-race
test-bench:   ARGS=-run=xxxxxMatchNothingxxxxx -test.benchtime=1ms -bench=.
test-short:   ARGS=-short
test-verbose: ARGS=-v -race
test-concurrent-safe: ARGS=-run=ConcurrentSafe -count=100 -race
test-concurrent-safe: TIMEOUT=120
$(TEST_TARGETS): test
test: $(OTEL_GO_MOD_DIRS:%=test/%)
test/%: DIR=$*
test/%:
	@echo "$(GO) test -timeout $(TIMEOUT)s $(ARGS) $(DIR)/..." \
		&& cd $(DIR) \
		&& $(GO) list ./... \
		| grep -v third_party \
		| xargs $(GO) test -timeout $(TIMEOUT)s $(ARGS)

COVERAGE_MODE    = atomic
COVERAGE_PROFILE = coverage.out
.PHONY: test-coverage
test-coverage: $(GOCOVMERGE)
	@set -e; \
	printf "" > coverage.txt; \
	for dir in $(ALL_COVERAGE_MOD_DIRS); do \
	  echo "$(GO) test -coverpkg=go.opentelemetry.io/otel/... -covermode=$(COVERAGE_MODE) -coverprofile="$(COVERAGE_PROFILE)" $${dir}/..."; \
	  (cd "$${dir}" && \
	    $(GO) list ./... \
	    | grep -v third_party \
	    | grep -v 'semconv/v.*' \
	    | xargs $(GO) test -coverpkg=./... -covermode=$(COVERAGE_MODE) -coverprofile="$(COVERAGE_PROFILE)" && \
	  $(GO) tool cover -html=coverage.out -o coverage.html); \
	done; \
	$(GOCOVMERGE) $$(find . -name coverage.out) > coverage.txt

.PHONY: benchmark
benchmark: $(OTEL_GO_MOD_DIRS:%=benchmark/%)
benchmark/%:
	@echo "$(GO) test -run=xxxxxMatchNothingxxxxx -bench=. $*..." \
		&& cd $* \
		&& $(GO) list ./... \
		| grep -v third_party \
		| xargs $(GO) test -run=xxxxxMatchNothingxxxxx -bench=.

.PHONY: golangci-lint golangci-lint-fix
golangci-lint-fix: ARGS=--fix
golangci-lint-fix: golangci-lint
golangci-lint: $(OTEL_GO_MOD_DIRS:%=golangci-lint/%)
golangci-lint/%: DIR=$*
golangci-lint/%: $(GOLANGCI_LINT)
	@echo 'golangci-lint $(if $(ARGS),$(ARGS) ,)$(DIR)' \
		&& cd $(DIR) \
		&& $(GOLANGCI_LINT) run --allow-serial-runners $(ARGS)

.PHONY: crosslink
crosslink: $(CROSSLINK)
	@echo "Updating intra-repository dependencies in all go modules" \
		&& $(CROSSLINK) --root=$(shell pwd) --prune

.PHONY: go-mod-tidy
go-mod-tidy: $(ALL_GO_MOD_DIRS:%=go-mod-tidy/%)
go-mod-tidy/%: DIR=$*
go-mod-tidy/%: crosslink
	@echo "$(GO) mod tidy in $(DIR)" \
		&& cd $(DIR) \
		&& $(GO) mod tidy -compat=1.21

.PHONY: lint-modules
lint-modules: go-mod-tidy

.PHONY: lint
lint: misspell lint-modules golangci-lint govulncheck

.PHONY: vanity-import-check
vanity-import-check: $(PORTO)
	@$(PORTO) --include-internal -l . || ( echo "(run: make vanity-import-fix)"; exit 1 )

.PHONY: misspell
misspell: $(MISSPELL)
	@$(MISSPELL) -w $(ALL_DOCS)

.PHONY: govulncheck
govulncheck: $(OTEL_GO_MOD_DIRS:%=govulncheck/%)
govulncheck/%: DIR=$*
govulncheck/%: $(GOVULNCHECK)
	@echo "govulncheck ./... in $(DIR)" \
		&& cd $(DIR) \
		&& $(GOVULNCHECK) ./...

.PHONY: codespell
codespell: $(CODESPELL)
	@$(DOCKERPY) $(CODESPELL)

.PHONY: toolchain-check
toolchain-check:
	@toolchainRes=$$(for f in $(ALL_GO_MOD_DIRS); do \
	           awk '/^toolchain/ { found=1; next } END { if (found) print FILENAME }' $$f/go.mod; \
	done); \
	if [ -n "$${toolchainRes}" ]; then \
			echo "toolchain checking failed:"; echo "$${toolchainRes}"; \
			exit 1; \
	fi

.PHONY: license-check
license-check:
	@licRes=$$(for f in $$(find . -type f \( -iname '*.go' -o -iname '*.sh' \) ! -path '**/third_party/*' ! -path './.git/*' ) ; do \
	           awk '/Copyright The OpenTelemetry Authors|generated|GENERATED/ && NR<=4 { found=1; next } END { if (!found) print FILENAME }' $$f; \
	   done); \
	   if [ -n "$${licRes}" ]; then \
	           echo "license header checking failed:"; echo "$${licRes}"; \
	           exit 1; \
	   fi

.PHONY: check-clean-work-tree
check-clean-work-tree:
	@if ! git diff --quiet; then \
	  echo; \
	  echo 'Working tree is not clean, did you forget to run "make precommit"?'; \
	  echo; \
	  git status; \
	  exit 1; \
	fi

SEMCONVPKG ?= "semconv/"
.PHONY: semconv-generate
semconv-generate: $(SEMCONVGEN) $(SEMCONVKIT)
	[ "$(TAG)" ] || ( echo "TAG unset: missing opentelemetry semantic-conventions tag"; exit 1 )
	[ "$(OTEL_SEMCONV_REPO)" ] || ( echo "OTEL_SEMCONV_REPO unset: missing path to opentelemetry semantic-conventions repo"; exit 1 )
	$(SEMCONVGEN) -i "$(OTEL_SEMCONV_REPO)/model/." --only=attribute_group -p conventionType=trace -f attribute_group.go -z "$(SEMCONVPKG)/capitalizations.txt" -t "$(SEMCONVPKG)/template.j2" -s "$(TAG)"
	$(SEMCONVGEN) -i "$(OTEL_SEMCONV_REPO)/model/." --only=metric  -f metric.go -t "$(SEMCONVPKG)/metric_template.j2" -s "$(TAG)"
	$(SEMCONVKIT) -output "$(SEMCONVPKG)/$(TAG)" -tag "$(TAG)"

.PHONY: gorelease
gorelease: $(OTEL_GO_MOD_DIRS:%=gorelease/%)
gorelease/%: DIR=$*
gorelease/%:| $(GORELEASE)
	@echo "gorelease in $(DIR):" \
		&& cd $(DIR) \
		&& $(GORELEASE) \
		|| echo ""

.PHONY: verify-mods
verify-mods: $(MULTIMOD)
	$(MULTIMOD) verify

.PHONY: prerelease
prerelease: verify-mods
	@[ "${MODSET}" ] || ( echo ">> env var MODSET is not set"; exit 1 )
	$(MULTIMOD) prerelease -m ${MODSET}

COMMIT ?= "HEAD"
.PHONY: add-tags
add-tags: verify-mods
	@[ "${MODSET}" ] || ( echo ">> env var MODSET is not set"; exit 1 )
	$(MULTIMOD) tag -m ${MODSET} -c ${COMMIT}

.PHONY: lint-markdown
lint-markdown:
	docker run -v "$(CURDIR):$(WORKDIR)" avtodev/markdown-lint:v1 -c $(WORKDIR)/.markdownlint.yaml $(WORKDIR)/**/*.md

.PHONY: verify-readmes
verify-readmes:
	./verify_readmes.sh
