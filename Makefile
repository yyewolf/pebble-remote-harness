.PHONY: all api api-test extension vsix watchapp companion package fmt clean help

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Toolchains install into $HOME without root — see docs/toolchain.md.
# Override any of these if yours live elsewhere.
JAVA_HOME ?= $(HOME)/.local/share/jdk/jdk-17.0.20+8
ANDROID_HOME ?= $(HOME)/Android/Sdk
PEBBLE ?= $(HOME)/.local/bin/pebble

export JAVA_HOME
export ANDROID_HOME

help:
	@echo "api        build the prh daemon"
	@echo "api-test   vet and test the Go module"
	@echo "extension  compile the VSCode extension"
	@echo "vsix       package the extension, prh binary included"
	@echo "watchapp   build the .pbw for emery"
	@echo "companion  build the Android debug APK"
	@echo "package    build every installable artifact into dist/"
	@echo "fmt        format everything that has a formatter available"
	@echo "clean      remove build output"

all: api extension watchapp companion

api:
	cd api && go build -ldflags "-X main.version=$(VERSION)" -o ../bin/prh ./cmd/prh

api-test:
	cd api && go vet ./... && go test ./...

extension:
	cd extension && npm install && npm run compile

# The VSIX carries prh itself, so installing the extension is the only step
# needed on a fresh machine. Without this copy the daemon is only found if it
# already happens to be on the box, which is exactly the assumption that makes
# a package untestable.
vsix: api extension
	mkdir -p extension/bin
	cp bin/prh extension/bin/prh
	rm -rf extension/plugin
	mkdir -p extension/plugin
	cp -r plugin/package.json plugin/src extension/plugin/
	cd extension && npx --yes @vscode/vsce package --out ../bin/

watchapp:
	cd watchapp && $(PEBBLE) build

# Needs companion/local.properties with sdk.dir, or ANDROID_HOME above.
companion:
	cd companion && ./gradlew assembleDebug

fmt:
	cd api && gofmt -w .

clean:
	rm -rf bin
	rm -rf extension/out
	rm -rf watchapp/build
	cd companion && ./gradlew clean || true
