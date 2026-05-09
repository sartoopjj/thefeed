.PHONY: all build build-server build-client test clean lint fmt vet \
	ios-bind ios-bind-catalyst ios-build ios-test ios-clean ios-list-sims ios-deps

BINARY_SERVER = thefeed-server
BINARY_CLIENT = thefeed-client
BUILD_DIR = build

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -s -w \
	-X github.com/sartoopjj/thefeed/internal/version.Version=$(VERSION) \
	-X github.com/sartoopjj/thefeed/internal/version.Commit=$(COMMIT) \
	-X github.com/sartoopjj/thefeed/internal/version.Date=$(DATE)

GOFLAGS = -trimpath -ldflags="$(LDFLAGS)"
export CGO_ENABLED = 0

# CLIENT_GOFLAGS appends the platform-specific AssetTemplate so the
# in-app GitHub update check (internal/update) can point users at the
# right published binary. {V} is replaced at runtime with the version
# string read from the public VERSION file. Pass the asset filename as
# the first argument.
#   $(call CLIENT_GOFLAGS,thefeed-client-{V}-linux-amd64)
CLIENT_GOFLAGS = -trimpath -ldflags="$(LDFLAGS) -X github.com/sartoopjj/thefeed/internal/version.AssetTemplate=$(1)"

all: test build

build: build-server build-client

build-server:
	@mkdir -p $(BUILD_DIR)
	go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_SERVER) ./cmd/server

build-client:
	@mkdir -p $(BUILD_DIR)
	go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_CLIENT) ./cmd/client

test:
	go test -race -count=1 ./...

lint: vet
	@command -v golangci-lint >/dev/null 2>&1 || echo "golangci-lint not found, skipping"
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || true

vet:
	go vet ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf $(BUILD_DIR)

# Cross-compilation targets
build-all: build-linux-amd64 build-linux-arm64 build-darwin-amd64 build-darwin-arm64 build-freebsd-amd64 build-freebsd-arm64 build-windows-amd64 build-android-arm64 build-android-arm

build-linux-amd64:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_SERVER)-linux-amd64 ./cmd/server
	GOOS=linux GOARCH=amd64 go build $(call CLIENT_GOFLAGS,thefeed-client-{V}-linux-amd64) -o $(BUILD_DIR)/$(BINARY_CLIENT)-linux-amd64 ./cmd/client

build-linux-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_SERVER)-linux-arm64 ./cmd/server
	GOOS=linux GOARCH=arm64 go build $(call CLIENT_GOFLAGS,thefeed-client-{V}-linux-arm64) -o $(BUILD_DIR)/$(BINARY_CLIENT)-linux-arm64 ./cmd/client

build-darwin-amd64:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_SERVER)-darwin-amd64 ./cmd/server
	GOOS=darwin GOARCH=amd64 go build $(call CLIENT_GOFLAGS,thefeed-client-{V}-darwin-amd64) -o $(BUILD_DIR)/$(BINARY_CLIENT)-darwin-amd64 ./cmd/client

build-darwin-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_SERVER)-darwin-arm64 ./cmd/server
	GOOS=darwin GOARCH=arm64 go build $(call CLIENT_GOFLAGS,thefeed-client-{V}-darwin-arm64) -o $(BUILD_DIR)/$(BINARY_CLIENT)-darwin-arm64 ./cmd/client

build-freebsd-amd64:
	@mkdir -p $(BUILD_DIR)
	GOOS=freebsd GOARCH=amd64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_SERVER)-freebsd-amd64 ./cmd/server
	GOOS=freebsd GOARCH=amd64 go build $(call CLIENT_GOFLAGS,thefeed-client-{V}-freebsd-amd64) -o $(BUILD_DIR)/$(BINARY_CLIENT)-freebsd-amd64 ./cmd/client

build-freebsd-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=freebsd GOARCH=arm64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_SERVER)-freebsd-arm64 ./cmd/server
	GOOS=freebsd GOARCH=arm64 go build $(call CLIENT_GOFLAGS,thefeed-client-{V}-freebsd-arm64) -o $(BUILD_DIR)/$(BINARY_CLIENT)-freebsd-arm64 ./cmd/client

build-windows-amd64:
	@mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY_SERVER)-windows-amd64.exe ./cmd/server
	GOOS=windows GOARCH=amd64 go build $(call CLIENT_GOFLAGS,thefeed-client-{V}-windows-amd64.exe) -o $(BUILD_DIR)/$(BINARY_CLIENT)-windows-amd64.exe ./cmd/client

build-android-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=android GOARCH=arm64 go build $(call CLIENT_GOFLAGS,thefeed-client-android-arm64) -o $(BUILD_DIR)/$(BINARY_CLIENT)-android-arm64 ./cmd/client

build-android-arm:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm GOARM=7 go build $(call CLIENT_GOFLAGS,thefeed-client-android-arm) -o $(BUILD_DIR)/$(BINARY_CLIENT)-android-arm ./cmd/client

# ===== iOS / Mac Catalyst =====
# Requires: Xcode + gomobile (go install golang.org/x/mobile/cmd/gomobile@latest && gomobile init)

IOS_DIR = ios
IOS_FRAMEWORK = $(IOS_DIR)/Mobile.xcframework
IOS_SCHEME = Thefeed
IOS_PROJECT = $(IOS_DIR)/Thefeed.xcodeproj
# Default simulator: pick the first available iPhone (override with IOS_SIM_NAME='iPhone 17').
IOS_SIM_NAME ?= $(shell xcrun simctl list devices available 2>/dev/null | awk -F'[()]' '/-- iOS [0-9]/{ios=1;next} /^-- /{ios=0} ios && /iPhone/{print $$1; exit}' | sed 's/^[[:space:]]*//;s/[[:space:]]*$$//')

ios-deps:
	@grep -q "golang.org/x/mobile" go.mod || go get golang.org/x/mobile/bind golang.org/x/mobile/bind/objc
	go mod tidy

ios-bind: ios-deps
	@command -v gomobile >/dev/null 2>&1 || { echo "gomobile not found. Run: go install golang.org/x/mobile/cmd/gomobile@latest && gomobile init"; exit 1; }
	gomobile bind -iosversion=14.0 -target=ios,iossimulator -o $(IOS_FRAMEWORK) ./mobile

ios-bind-catalyst: ios-deps
	@command -v gomobile >/dev/null 2>&1 || { echo "gomobile not found"; exit 1; }
	gomobile bind -iosversion=14.0 -target=ios,iossimulator,maccatalyst -o $(IOS_FRAMEWORK) ./mobile

ios-list-sims:
	xcrun simctl list devices available

ios-build: $(IOS_FRAMEWORK)
	xcodebuild -project $(IOS_PROJECT) -scheme $(IOS_SCHEME) \
		-destination 'platform=iOS Simulator,name=$(IOS_SIM_NAME)' \
		build

ios-test: $(IOS_FRAMEWORK)
	xcodebuild test -project $(IOS_PROJECT) -scheme $(IOS_SCHEME) \
		-destination 'platform=iOS Simulator,name=$(IOS_SIM_NAME)'

$(IOS_FRAMEWORK):
	$(MAKE) ios-bind

ios-clean:
	rm -rf $(IOS_FRAMEWORK) $(IOS_DIR)/build $(IOS_DIR)/DerivedData

# UPX compression (requires upx in PATH) — only for Linux/Windows binaries
upx:
	@command -v upx >/dev/null 2>&1 || { echo "upx not found, skipping compression"; exit 0; }
	@for f in $(BUILD_DIR)/$(BINARY_SERVER)-linux-* $(BUILD_DIR)/$(BINARY_CLIENT)-linux-* \
	          $(BUILD_DIR)/$(BINARY_SERVER)-windows-*.exe $(BUILD_DIR)/$(BINARY_CLIENT)-windows-*.exe \
	          $(BUILD_DIR)/$(BINARY_CLIENT)-android-*; do \
		if [ -f "$$f" ]; then echo "UPX: $$f"; upx --best --lzma "$$f" || true; fi \
	done
