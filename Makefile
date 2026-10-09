# Go はローカルに入れず docker の golang:1.26 で動かす。
GO_IMAGE   ?= golang:1.26
LINT_IMAGE ?= golangci/golangci-lint:v2.14.0
IMAGE      ?= ghcr.io/morinoparty/moripa-worlds:dev
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

CACHE  ?= $(CURDIR)/.cache
DOCKER := docker run --rm -u $(shell id -u):$(shell id -g) -e HOME=/tmp \
	-e GOMODCACHE=/cache/mod -e GOCACHE=/cache/build -e GOLANGCI_LINT_CACHE=/cache/lint \
	-v $(CACHE):/cache -v $(CURDIR):/src -w /src

.PHONY: test test-integration vet lint fmt build image tidy

test: ## 単体テスト (race 検出つき)
	@mkdir -p $(CACHE)
	$(DOCKER) $(GO_IMAGE) go test -race ./...

test-integration: ## RustFS を docker で起動して統合テストを流す
	@mkdir -p $(CACHE)
	docker rm -f moripa-worlds-rustfs >/dev/null 2>&1 || true
	docker run -d --name moripa-worlds-rustfs -p 127.0.0.1:39000:9000 \
		-e RUSTFS_ACCESS_KEY=mwtestaccess -e RUSTFS_SECRET_KEY=mwtestsecret123 rustfs/rustfs:1.0.1
	sleep 5
	$(DOCKER) --network host -e S3_ENDPOINT=http://127.0.0.1:39000 \
		-e AWS_ACCESS_KEY_ID=mwtestaccess -e AWS_SECRET_ACCESS_KEY=mwtestsecret123 \
		$(GO_IMAGE) go test -tags integration -run Integration -count=1 -v ./internal/schematic/; \
		status=$$?; docker rm -f moripa-worlds-rustfs >/dev/null; exit $$status

vet:
	@mkdir -p $(CACHE)
	$(DOCKER) $(GO_IMAGE) go vet ./...

lint:
	@mkdir -p $(CACHE)
	$(DOCKER) $(LINT_IMAGE) golangci-lint run ./...

fmt:
	@mkdir -p $(CACHE)
	$(DOCKER) $(LINT_IMAGE) golangci-lint fmt ./...

tidy:
	@mkdir -p $(CACHE)
	$(DOCKER) $(GO_IMAGE) go mod tidy

build: ## bin/moripa-worlds を作る
	@mkdir -p $(CACHE) bin
	$(DOCKER) -e CGO_ENABLED=0 $(GO_IMAGE) go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/moripa-worlds ./cmd/moripa-worlds

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .
