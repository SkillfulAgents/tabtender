.PHONY: build test e2e clients app lint clean

build: ## Build tabtenderd for this machine
	go build -o bin/tabtenderd ./cmd/tabtenderd

test: ## Unit tests
	go test -race ./...

e2e: ## End-to-end tests against a real browser (see test/e2e/harness_test.go)
	go test -tags e2e -count=1 -v ./test/e2e/

clients: build ## Playwright and Puppeteer conformance suite
	cd test/clients && npm ci && node run.mjs ../../bin/tabtenderd

app: ## Build dist/Tabtender.app and a DMG (macOS)
	packaging/macos/build-app.sh --dmg

lint:
	gofmt -l . && go vet -tags e2e ./... && golangci-lint run

clean:
	rm -rf bin dist apps/macos/.build
