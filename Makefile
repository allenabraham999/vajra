.PHONY: build test lint clean benchmark vet fmt tidy run-master run-agent run-proxy db-up db-down all

BIN_DIR := bin
GO := go
GOFLAGS := -trimpath
LDFLAGS := -s -w

BINARIES := vajra-agent vajra-master vajra-proxy vajra

all: build

build: $(BINARIES:%=$(BIN_DIR)/%)

$(BIN_DIR)/%:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $@ ./cmd/$*

test:
	$(GO) test ./...

test-integration:
	$(GO) test -tags=integration ./test/...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

lint: vet
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint not installed; skipping"

tidy:
	$(GO) mod tidy

benchmark:
	$(GO) test -bench=. -benchmem ./internal/vmm/...

clean:
	rm -rf $(BIN_DIR)
	$(GO) clean -cache -testcache

db-up:
	docker compose up -d postgres redis

db-down:
	docker compose down

run-master: $(BIN_DIR)/vajra-master
	./$(BIN_DIR)/vajra-master

run-agent: $(BIN_DIR)/vajra-agent
	./$(BIN_DIR)/vajra-agent

run-proxy: $(BIN_DIR)/vajra-proxy
	./$(BIN_DIR)/vajra-proxy
