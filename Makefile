# all2api — local parity targets
# Purpose: make `make check` reproduce exactly what CI runs, so backend does not
# iterate against GitHub Actions (each round trip costs minutes and a push).
# Owner: devops. Repo path: Makefile

BINARY      ?= all2api
ENTRYPOINT  ?= ./cmd/gateway
EMBED_DIR   ?= internal/web/dist
PANEL_DIR   ?= web-panel
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS     := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: help deps tidy embed panel lint vet test race build build-all docker docker-smoke fpk check clean scan

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

deps: ## download module deps
	go mod download

tidy: ## keep go.mod (and go.sum, once deps exist) honest — run before committing
	go mod tidy
	-@git diff --exit-code -- go.mod $$(test -f go.sum && echo go.sum) \
		|| echo "go.mod/go.sum changed — commit them"

embed: ## ensure the go:embed target exists so the build can compile
	@mkdir -p $(EMBED_DIR)
	@test -f $(EMBED_DIR)/index.html || @test -f $(EMBED_DIR)/.gitkeep || touch $(EMBED_DIR)/.gitkeep

panel: ## build the web panel (no-op while the bundle script is still an M1 item)
	@test -f $(PANEL_DIR)/package.json || { echo "no panel yet — skipping"; exit 0; }
	cd $(PANEL_DIR) && npm ci && npm run --if-present build
	@test -n "$$(find $(PANEL_DIR)/dist -type f 2>/dev/null | head -1)" \
		|| { echo "panel produced no dist/ — embed dir left untouched"; exit 0; }
	rm -rf $(EMBED_DIR) && mkdir -p $(EMBED_DIR)
	cp -r $(PANEL_DIR)/dist/* $(EMBED_DIR)/

vet: ## go vet
	go vet ./...

test: ## unit tests
	go test -count=1 ./...

race: ## unit tests with the race detector (this is the CI gate)
	go test -race -count=1 -timeout 300s ./...

lint: ## golangci-lint (install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8)
	golangci-lint run --timeout 5m

build: embed ## build for the host platform
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY) $(ENTRYPOINT)

build-all: embed ## cross-compile the four shipped targets
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64  $(ENTRYPOINT)
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64  $(ENTRYPOINT)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-windows-amd64.exe $(ENTRYPOINT)
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-arm64 $(ENTRYPOINT)
	cd dist && sha256sum $(BINARY)-* > SHA256SUMS.txt
	@echo "artifacts + checksums in ./dist"

docker: ## build the image for the host arch
	docker build -t $(BINARY):dev .

docker-smoke: docker ## run the container and assert /healthz (M0 acceptance)
	@mkdir -p auths data
	@test -f config.json || cp config.example.json config.json
	docker rm -f $(BINARY)-smoke 2>/dev/null || true
	docker run -d --name $(BINARY)-smoke -p 7863:7863 \
	  -v $$PWD/config.json:/app/config.json:ro \
	  -v $$PWD/auths:/app/auths -v $$PWD/data:/app/data \
	  -e TZ=Asia/Shanghai $(BINARY):dev
	@i=0; while [ $$i -lt 30 ]; do \
	  s=$$(docker inspect --format '{{.State.Health.Status}}' $(BINARY)-smoke 2>/dev/null || echo unknown); \
	  h=$$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:7863/healthz || echo 000); \
	  echo "attempt $$i: health=$$s http=$$h"; \
	  if [ "$$s" = "healthy" ] && [ "$$h" = "200" ]; then docker rm -f $(BINARY)-smoke >/dev/null; echo "SMOKE OK"; exit 0; fi; \
	  i=$$(($$i+1)); sleep 2; \
	done; \
	docker logs $(BINARY)-smoke; docker rm -f $(BINARY)-smoke >/dev/null; \
	echo "SMOKE FAILED"; exit 1

scan: ## local security scan before pushing (mirrors the CI security job)
	gosec -severity high ./...
	govulncheck ./...

workflows: ## lint the Actions workflows offline (needs actionlint)
	@actionlint .github/workflows/*.yml || { \
		echo "actionlint missing — go install github.com/rhysd/actionlint/cmd/actionlint@latest"; \
		echo "it catches the class of error GitHub rejects at push time (e.g. hashFiles in a job-level if)"; \
		exit 1; \
	}

fpk: ## package the fnOS .fpk app (see deploy/fnos/README.md for prerequisites)
	bash deploy/fnos/package-fpk.sh

check: vet lint race build build-all docker-smoke ## the full pre-push gate

clean:
	rm -rf dist staging coverage.out
