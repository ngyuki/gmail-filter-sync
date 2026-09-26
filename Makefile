BINARY	:= gmail-filter-sync
TARGET  := dist/${BINARY}
VERSION	:= $(shell git describe --tags 2>/dev/null)

.PHONY: all
all: check test build

.PHONY: build
build:
	mkdir -p dist
	go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o ${TARGET} .

.PHONY: test
test:
	go test ./...

.PHONY: check
check:
	go vet ./...
	govulncheck ./...
	staticcheck ./...
	actionlint

.PHONY: clean
clean:
	rm -fr dist/
