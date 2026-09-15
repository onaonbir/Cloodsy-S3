APP_NAME := cloodsys3
BUILD_DIR := build
MODULE := github.com/onaonbir/Cloodsy-S3

VERSION := $(shell cat VERSION 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

# Pure-Go stack (modernc.org/sqlite): cgo is never needed. Forcing it off makes
# every binary fully static so it runs on any glibc/musl system.
export CGO_ENABLED = 0

GOFLAGS_BUILD := -trimpath
LDFLAGS := -s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.CommitHash=$(COMMIT)' \
	-X 'main.BuildDate=$(BUILD_DATE)'

GO_BUILD = go build $(GOFLAGS_BUILD) -ldflags "$(LDFLAGS)"

.PHONY: build clean run version fmt vet lint test vuln check \
	build-all build-linux build-windows build-pi build-arm64 build-armv7 build-mac build-mac-intel

# --- Default: current platform ---

build:
	@mkdir -p $(BUILD_DIR)
	$(GO_BUILD) -o $(BUILD_DIR)/$(APP_NAME) .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME) v$(VERSION) ($(COMMIT))"

# --- Cross-compile targets ---

build-linux:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GO_BUILD) -o $(BUILD_DIR)/$(APP_NAME)-linux-amd64 .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-linux-amd64 v$(VERSION)"

build-windows:
	@mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 $(GO_BUILD) -o $(BUILD_DIR)/$(APP_NAME)-windows-amd64.exe .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-windows-amd64.exe v$(VERSION)"

build-pi: build-arm64
	@echo "Raspberry Pi build ready: $(BUILD_DIR)/$(APP_NAME)-linux-arm64"

build-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 $(GO_BUILD) -o $(BUILD_DIR)/$(APP_NAME)-linux-arm64 .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-linux-arm64 v$(VERSION)"

build-armv7:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm GOARM=7 $(GO_BUILD) -o $(BUILD_DIR)/$(APP_NAME)-linux-armv7 .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-linux-armv7 v$(VERSION)"

build-mac:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 $(GO_BUILD) -o $(BUILD_DIR)/$(APP_NAME)-darwin-arm64 .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-darwin-arm64 v$(VERSION)"

build-mac-intel:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 $(GO_BUILD) -o $(BUILD_DIR)/$(APP_NAME)-darwin-amd64 .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-darwin-amd64 v$(VERSION)"

# --- Build all platforms ---

build-all: build-linux build-arm64 build-armv7
	@echo ""
	@echo "All builds complete (v$(VERSION)):"
	@ls -lh $(BUILD_DIR)/$(APP_NAME)-* 2>/dev/null
	@echo ""

# --- Quality ---

fmt:
	gofmt -l -w .

vet:
	go vet ./...

# staticcheck is optional: install with `go install honnef.co/go/tools/cmd/staticcheck@latest`
lint:
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; \
	else echo "staticcheck not installed; skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"; fi

test:
	go test ./...

vuln:
	@if command -v govulncheck >/dev/null 2>&1; then govulncheck ./...; \
	else go run golang.org/x/vuln/cmd/govulncheck@latest ./...; fi

check: vet lint test vuln

# --- Utility ---

# Only build artifacts are removed. Runtime data (./.cloodsys3: database and
# objects) is never touched by make.
clean:
	rm -rf $(BUILD_DIR)

# Runs from the repository root so the default data dir is ./.cloodsys3
# (gitignored) instead of something under build/.
run: build
	./$(BUILD_DIR)/$(APP_NAME) serve

version:
	@echo "$(VERSION)"
