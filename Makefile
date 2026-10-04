.PHONY: build test vet fmt tidy run image clean

BIN := bin/autobench

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

clean:
	rm -rf bin
