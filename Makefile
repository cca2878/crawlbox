GO ?= go
GOFMT ?= gofmt
VERSION ?= 0.1.0
KOPIA_VERSION := v0.23.1
GOVULNCHECK_VERSION := v1.1.4
override export CGO_ENABLED := 0
.PHONY: check test build integration tools vuln clean
check:
	GO=$(GO) GOFMT=$(GOFMT) sh scripts/check.sh
test:
	$(GO) test ./...
build:
	mkdir -p bin
	$(GO) build -trimpath -o bin/manager ./cmd/manager
	GOOS=wasip1 GOARCH=wasm $(GO) build -trimpath -buildmode=c-shared -o bin/fixture.wasm ./cmd/testplugin
	GO=$(GO) sh scripts/check-binary.sh bin/manager
tools:
	mkdir -p bin
	GOBIN=$(CURDIR)/bin $(GO) install github.com/kopia/kopia@$(KOPIA_VERSION)
	GO=$(GO) sh scripts/check-binary.sh bin/kopia
integration: build tools
	FIXTURE_WASM=$(CURDIR)/bin/fixture.wasm KOPIA_BIN=$(CURDIR)/bin/kopia $(GO) test -count=1 ./internal/app ./internal/kopia ./internal/bootstrap
vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
clean:
	rm -rf bin dist
