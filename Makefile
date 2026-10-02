.PHONY: build test vet fmt tidy run clean

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

clean:
	rm -rf bin
