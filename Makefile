BIN := local/bin

.PHONY: all build test vet clean

all: build

build: $(BIN)/gate $(BIN)/projector $(BIN)/pipeline $(BIN)/egress-listener

$(BIN)/gate: $(shell find cmd internal -name '*.go' 2>/dev/null)
	go build -o $@ ./cmd/gate

$(BIN)/projector: $(shell find cmd internal -name '*.go' 2>/dev/null)
	go build -o $@ ./cmd/projector

$(BIN)/pipeline: $(shell find cmd internal standards -type f 2>/dev/null)
	go build -o $@ ./cmd/pipeline

$(BIN)/egress-listener: $(shell find cmd internal -name '*.go' 2>/dev/null)
	go build -o $@ ./cmd/egress-listener

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf $(BIN)
