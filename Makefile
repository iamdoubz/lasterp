.PHONY: dev lint test build

dev:
	@command -v pnpm >/dev/null 2>&1 || npm install -g pnpm@9.15.0
	pnpm install
	go run ./cmd/lasterp dev

GOLANGCI_LINT_VERSION := v2.12.2

lint:
	gofmt -l . | grep -v '^$$' && exit 1 || true
	go vet ./...
	command -v golangci-lint >/dev/null 2>&1 || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	golangci-lint run ./...
	pnpm --dir web run lint
	./scripts/spdx-lint.sh
	./scripts/i18n-lint.sh

test:
	go test ./...
	pnpm --dir web run test
	./scripts/lint-checks_test.sh
	./scripts/i18n-lint_test.sh

build:
	go build -o bin/lasterp ./cmd/lasterp
	pnpm --dir web run build
	docker build -f deploy/Dockerfile -t lasterp:dev .
