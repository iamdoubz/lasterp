.PHONY: dev lint test build

dev:
	@command -v pnpm >/dev/null 2>&1 || (corepack enable 2>/dev/null || npm install -g pnpm)
	pnpm install
	go run ./cmd/lasterp dev

lint:
	gofmt -l . | grep -v '^$$' && exit 1 || true
	go vet ./...
	pnpm --dir web run lint
	./scripts/spdx-lint.sh

test:
	go test ./...
	pnpm --dir web run test
	./scripts/lint-checks_test.sh

build:
	go build -o bin/lasterp ./cmd/lasterp
	pnpm --dir web run build
	docker build -f deploy/Dockerfile -t lasterp:dev .
