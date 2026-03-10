.PHONY: build test integration-test lint lint-fix total-coverage

build:
	go build -o copilot-autodev main.go

test:
	go test ./...

integration-test:
	go test -tags=integration ./test/integration/...

lint:
	golangci-lint run

lint-fix:
	golangci-lint run --fix

total-coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
