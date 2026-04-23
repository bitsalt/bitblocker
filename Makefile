BINARY := bitblocker
PKG    := ./...

.PHONY: all build test lint vuln tidy clean

all: build

build:
	go build -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test -race -coverprofile=coverage.out $(PKG)

lint:
	golangci-lint run

vuln:
	govulncheck $(PKG)

tidy:
	go mod tidy

clean:
	rm -rf bin coverage.out
