# Build, test, lint, and format the repository.
.PHONY: build test lint lint-check format format-check verify git-hooks tools

# Tool versions. The tools target installs a tool that is missing or at another version, so
# these are the only places the versions are written down.
GOLANGCI_LINT_VERSION=v2.13.2
SHFMT_VERSION=v3.14.1
RUFF_VERSION=0.16.8
PYLINT_VERSION=4.0.8
# shellcheck-py appends its own release number to the shellcheck version it ships.
SHELLCHECK_VERSION=0.10.0
SHELLCHECK_PY_VERSION=0.10.0.1

# The tools target installs into the Go and uv tool directories. Prepend them so a recipe
# that just installed a tool can run it, whatever the caller's PATH holds.
GO_BIN := $(if $(shell command -v go 2>/dev/null),$(shell go env GOPATH 2>/dev/null)/bin)
UV_BIN := $(if $(shell command -v uv 2>/dev/null),$(shell uv tool dir --bin 2>/dev/null))
export PATH := $(if $(GO_BIN),$(GO_BIN):)$(if $(UV_BIN),$(UV_BIN):)$(PATH)

tools:
	@command -v golangci-lint > /dev/null 2>&1 && golangci-lint --version 2>/dev/null | grep -Fqw "$(GOLANGCI_LINT_VERSION:v%=%)" || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@command -v shfmt > /dev/null 2>&1 && shfmt --version 2>/dev/null | grep -Fqw "$(SHFMT_VERSION)" || go install mvdan.cc/sh/v3/cmd/shfmt@$(SHFMT_VERSION)
	@command -v uv > /dev/null 2>&1 || { echo 'uv is required to install the Python tools; see https://docs.astral.sh/uv/' >&2; exit 1; }
	@ruff --version 2>/dev/null | grep -Fqw "$(RUFF_VERSION)" || uv tool install --force --quiet ruff==$(RUFF_VERSION)
	@pylint --version 2>/dev/null | grep -Fqw "$(PYLINT_VERSION)" || uv tool install --force --quiet pylint==$(PYLINT_VERSION)
	@shellcheck --version 2>/dev/null | grep -Fqw "$(SHELLCHECK_VERSION)" || uv tool install --force --quiet shellcheck-py==$(SHELLCHECK_PY_VERSION)

# Command listing the shell scripts that shellcheck and shfmt cover: every tracked *.sh,
# plus every tracked executable whose first line is a shell shebang, because the container
# entrypoints and the VNC helpers carry no .sh suffix.
SHELL_SCRIPTS = git ls-files '*.sh'; git ls-files -s | awk '$$1 == "100755" { print $$4 }' | while IFS= read -r f; do case "$$(head -n 1 "$$f")" in *'/sh'* | *'/bash'* | *'env sh'* | *'env bash'*) printf '%s\n' "$$f";; esac; done


# methodfilecheck (see .golangci.yml) is a golangci-lint module plugin, so the
# Go linting must run through the custom binary built from the published
# plugin module.
custom-gcl: .custom-gcl.yml
	@golangci-lint custom --version $(GOLANGCI_LINT_VERSION)

build:
	@go build ./...

test:
	@go test ./...

lint-check: tools custom-gcl
	@./custom-gcl run --show-stats=false ./...
	@pylint --score=n .
	@ruff check --quiet .
	@files=$$($(SHELL_SCRIPTS)); [ -z "$$files" ] || shellcheck -x $$files
	@python3 scripts/lint_binaries.py
	@python3 scripts/update_agents_file_index.py --check

# Apply the autofixes, then report what is left to fix by hand.
lint: tools custom-gcl
	@./custom-gcl run --show-stats=false ./... --fix
	@ruff check --quiet --fix .
	@ruff format --quiet .
	@python3 scripts/update_agents_file_index.py
	@$(MAKE) --no-print-directory lint-check

# Apply and verify the shared formatters: gofmt and goimports through
# golangci-lint for Go, ruff format for the Python scripts, and shfmt for the
# shell scripts.
format: tools
	@golangci-lint fmt
	@ruff format --quiet .
	@files=$$($(SHELL_SCRIPTS)); [ -z "$$files" ] || shfmt -w $$files

format-check: tools
	@out=$$(golangci-lint fmt --diff); [ -z "$$out" ] || { echo 'Go files need formatting (gofmt, goimports):' >&2; echo "$$out" >&2; exit 1; }
	@ruff format --check --quiet .
	@files=$$($(SHELL_SCRIPTS)); [ -z "$$files" ] || { out=$$(shfmt -l $$files); [ -z "$$out" ] || { echo 'Shell files need shfmt:' >&2; echo "$$out" >&2; exit 1; }; }

verify: format-check lint-check

git-hooks:
	@./scripts/install-git-hooks.sh
