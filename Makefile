APP_NAME=aegora
VERSION?=dev

.PHONY: verify
verify: fmt tidy lint test build

.PHONY: all
all: fmt lint test build

.PHONY: fmt
fmt:
	go fmt ./...
	go vet ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: test
test:
	go test -race -cover ./...

.PHONY: build
build:
	go build \
	-trimpath \
	-buildvcs=true \
	-ldflags="-s -w -X github.com/syncronix-labs/aegora/internal/admin.Version=$(VERSION)" \
	-o bin/$(APP_NAME) \
	./cmd/aegora

.PHONY: run
run:
	go run ./cmd/aegora

.PHONY: clean
clean:
	rm -rf bin

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: docker-build
docker-build:
	docker build -f deployments/Dockerfile --build-arg VERSION=$(VERSION) -t aegora:local .

.PHONY: compose-up
compose-up:
	docker compose -f deployments/docker-compose.yml up --build -d

.PHONY: compose-down
compose-down:
	docker compose -f deployments/docker-compose.yml down