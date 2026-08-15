.PHONY: all api api-test api-cross extension vsix watchapp companion package fmt clean help

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Platforms the daemon is released for. CGO is off everywhere, so each of
# these is a static binary produced by the host toolchain — no cross compiler
# and no container per target.
#
# windows/amd64 is built and shipped, but only for running prh standalone: the
# VSCode extension is not released for Windows because Hop 0 is an AF_UNIX
# socket and Node's net module cannot speak AF_UNIX there, only named pipes.
PRH_PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

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
	@echo "api-cross  build the daemon for every released platform"
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

# One binary per entry in PRH_PLATFORMS, named after its target so the whole
# directory can be attached to a release as-is. Doubles as the cheapest
# possible check that no platform-specific build tag has crept in.
api-cross:
	@mkdir -p bin/release
	@for target in $(PRH_PLATFORMS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		( cd api && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" \
			-o ../bin/release/prh-$$os-$$arch$$ext ./cmd/prh ) || exit 1; \
	done
	@ls -la bin/release/

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
	cp LICENSE extension/LICENSE
	cd extension && npx --yes @vscode/vsce package --out ../bin/

watchapp:
	cd watchapp && $(PEBBLE) build

# Needs companion/local.properties with sdk.dir, or ANDROID_HOME above.
companion:
	cd companion && ./gradlew assembleDebug

# Everything a fresh machine needs, in one directory. The VSIX already
# contains prh and the Kilo plugin; the standalone binary is here for running
# the daemon without VSCode, which is a supported setup.
package: vsix watchapp companion
	mkdir -p dist
	cp bin/prh dist/prh
	cp bin/pebble-remote-harness-*.vsix dist/
	cp watchapp/build/watchapp.pbw dist/prh-watchapp.pbw
	cp companion/app/build/outputs/apk/debug/app-debug.apk dist/prh-companion.apk
	@echo
	@ls -la dist/

fmt:
	cd api && gofmt -w .

clean:
	rm -rf bin dist
	rm -rf extension/out extension/bin extension/plugin extension/LICENSE
	rm -rf watchapp/build
	cd companion && ./gradlew clean || true
