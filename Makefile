APP_NAME := cloodsys3
BUILD_DIR := build
MODULE := github.com/onaonbir/Cloodsy-S3

VERSION := $(shell cat VERSION 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.CommitHash=$(COMMIT)' \
	-X 'main.BuildDate=$(BUILD_DATE)'

.PHONY: build clean run version build-all build-linux build-windows build-pi build-arm64 build-armv7 build-mac build-mac-intel

# --- Default: current platform ---

build:
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME) .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME) v$(VERSION) ($(COMMIT))"

# --- Cross-compile targets ---

build-linux:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-linux-amd64 .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-linux-amd64 v$(VERSION)"

build-windows:
	@mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-windows-amd64.exe .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-windows-amd64.exe v$(VERSION)"

build-pi: build-arm64
	@echo "Raspberry Pi build ready: $(BUILD_DIR)/$(APP_NAME)-linux-arm64"

build-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-linux-arm64 .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-linux-arm64 v$(VERSION)"

build-armv7:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-linux-armv7 .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-linux-armv7 v$(VERSION)"

build-mac:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-darwin-arm64 .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-darwin-arm64 v$(VERSION)"

build-mac-intel:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-darwin-amd64 .
	@echo "Build: $(BUILD_DIR)/$(APP_NAME)-darwin-amd64 v$(VERSION)"

# --- Build all platforms ---

build-all: build-linux build-arm64 build-armv7
	@echo ""
	@echo "All builds complete (v$(VERSION)):"
	@ls -lh $(BUILD_DIR)/$(APP_NAME)-* 2>/dev/null
	@echo ""

# --- Utility ---

clean:
	rm -rf $(BUILD_DIR)

run: build
	cd $(BUILD_DIR) && ./$(APP_NAME) serve

version:
	@echo "$(VERSION)"
