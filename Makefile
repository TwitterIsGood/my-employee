BIN := local/bin

.PHONY: all build test vet check-leaks clean

all: build

build: $(BIN)/gate $(BIN)/projector $(BIN)/pipeline $(BIN)/stage $(BIN)/chain $(BIN)/egress-listener

$(BIN)/gate: $(shell find cmd internal -name '*.go' 2>/dev/null)
	go build -o $@ ./cmd/gate

$(BIN)/projector: $(shell find cmd internal -name '*.go' 2>/dev/null)
	go build -o $@ ./cmd/projector

$(BIN)/pipeline: $(shell find cmd internal standards -type f 2>/dev/null)
	go build -o $@ ./cmd/pipeline

$(BIN)/stage: $(shell find cmd internal standards -type f 2>/dev/null)
	go build -o $@ ./cmd/stage

$(BIN)/chain: $(shell find cmd internal standards -type f 2>/dev/null)
	go build -o $@ ./cmd/chain

$(BIN)/egress-listener: $(shell find cmd internal -name '*.go' 2>/dev/null)
	go build -o $@ ./cmd/egress-listener

test:
	go test ./...

vet:
	go vet ./...

# 这个仓库是公开的。提交前先过这一道——`make test` 里也含它，
# 单独列出来只是为了能只跑它。见 standards/leaks.md。
check-leaks:
	go test ./internal/leaks/ -v -run 'NoLeaks|CatchesLeaks|LetsNormal'

clean:
	rm -rf $(BIN)
