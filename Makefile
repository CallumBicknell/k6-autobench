.PHONY: build test vet fmt tidy run image release clean

BIN := bin/autobench

# The release build records which build it is: the commit and the time. It does
# not override the version, which is the constant the README documents and the
# version guard checks -- letting a tag rewrite it would put the published
# statement and the binary out of step.
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILT_AT ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
  -X main.commit=$(COMMIT) \
  -X main.builtAt=$(BUILT_AT)

# VERSION names the artefacts. It is read from the source, so a filename cannot
# disagree with what the binary reports.
VERSION  := $(shell sed -n 's/^var version = "\(.*\)"/\1/p' cmd/autobench/main.go)

build:
	go build -o $(BIN) ./cmd/autobench

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

run: build
	./$(BIN) run examples/api.yaml

# Build the container, which ships the orchestrator and the generator together.
image:
	docker build -t autobench .

# Cross-compile the release binaries. autobench is pure Go and depends on nothing
# at runtime but k6, so each target is one static binary.
release:
	@rm -rf dist && mkdir -p dist
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  name=autobench_$(VERSION)_$${os}_$${arch}; \
	  ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	  echo "building $$name"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
	    -ldflags "$(LDFLAGS)" -o "dist/$$name$$ext" ./cmd/autobench || exit 1; \
	done
	@cd dist && sha256sum * > checksums.txt && cat checksums.txt

clean:
	rm -rf bin dist
