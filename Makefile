.PHONY: build test test-nocgo clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/loom ./cmd/loom

# -race needs cgo; build stays CGO_ENABLED=0. Force cgo on for the race run so an exported
# CGO_ENABLED=0 does not break it; test-nocgo is the toolchain-free fallback.
test:
	CGO_ENABLED=1 go test -v -race ./...

test-nocgo:
	CGO_ENABLED=0 go test ./...

clean:
	rm -rf bin/ loom
