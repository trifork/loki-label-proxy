.PHONY: build test lint vet fmt tidy docker

BINARY := loki-label-proxy
IMAGE  ?= ghcr.io/trifork/loki-label-proxy
TAG    ?= dev

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test -race -cover ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

docker:
	docker build -t $(IMAGE):$(TAG) .
