.PHONY: build test lint clean benchmark vet fmt tidy run-master run-agent run-proxy db-up db-down all stage-ch

BIN_DIR := bin
GO := go
GOFLAGS := -trimpath
LDFLAGS := -s -w

BINARIES := vajra-agent vajra-master vajra-proxy vajra

all: build

# build is .PHONY so it always invokes go build; the Go toolchain has its
# own per-package cache, so a no-op rebuild is cheap. The previous
# pattern-rule version skipped rebuilds whenever bin/<name> existed,
# which silently shipped stale binaries on deploy.
build: stage-ch
	@mkdir -p $(BIN_DIR)
	@for b in $(BINARIES); do \
	    echo "  building $$b..."; \
	    $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$$b ./cmd/$$b; \
	done

# stage-ch copies the locally-installed cloud-hypervisor binary into bin/
# so master can serve it from /internal/binaries/cloud-hypervisor to
# autoscaled nodes. Silent no-op when the binary isn't installed (dev
# laptops, CI without KVM).
stage-ch:
	@mkdir -p $(BIN_DIR)
	@cp /usr/local/bin/cloud-hypervisor $(BIN_DIR)/cloud-hypervisor 2>/dev/null || true

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
