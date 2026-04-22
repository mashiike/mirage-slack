.PHONY: build test tidy clean

BINARY := mirage-slack

build:
	go build -o $(BINARY) ./cmd/mirage-slack

test:
	go test ./...

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
