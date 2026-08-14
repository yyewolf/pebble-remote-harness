.PHONY: all api api-test extension watchapp companion fmt clean help

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

help:
	@echo "api        build the prh daemon"
	@echo "api-test   vet and test the Go module"
	@echo "extension  compile the VSCode extension (needs npm)"
	@echo "watchapp   build the .pbw (needs the Pebble SDK)"
	@echo "companion  build the Android APK (needs JDK + Android SDK)"
	@echo "fmt        format everything that has a formatter available"
	@echo "clean      remove build output"

all: api

api:
	cd api && go build -ldflags "-X main.version=$(VERSION)" -o ../bin/prh ./cmd/prh

api-test:
	cd api && go vet ./... && go test ./...

extension:
	cd extension && npm install && npm run compile

# Targets emery only. Requires pebble-tool; there is no SDK in this checkout.
watchapp:
	cd watchapp && pebble build

companion:
	cd companion && ./gradlew assembleDebug

fmt:
	cd api && gofmt -w .

clean:
	rm -rf bin
	rm -rf extension/out
	rm -rf watchapp/build
	rm -rf companion/app/build
