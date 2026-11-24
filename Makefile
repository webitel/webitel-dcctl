BUF        ?= go run github.com/bufbuild/buf/cmd/buf@v1.75.0
API_LINTER ?= go run github.com/googleapis/api-linter/v2/cmd/api-linter@v2.4.1

PROTO_DIR   := proto
PROTO_FILES := $(shell cd $(PROTO_DIR) && find . -name '*.proto' | sed 's|^\./||' | sort)
GEN_GO_DIR  := gen/go/dcctl/v1
SWAGGER     := api/dcctl.swagger.json
GENERATED   := $(GEN_GO_DIR) $(SWAGGER)
PROTO_IMAGE := build/dcctl.binpb

# Git ref the breaking check compares with, for example origin/main in CI.
BREAKING_AGAINST ?= main

.PHONY: proto proto-lint proto-check proto-breaking lint test

## proto: format the proto files and regenerate gen/ and api/, dropping outputs of removed files
proto:
	$(BUF) format -w
	rm -f $(GEN_GO_DIR)/*.go $(SWAGGER)
	$(BUF) generate

## proto-lint: buf format check, buf lint and api-linter
proto-lint:
	$(BUF) format --diff --exit-code
	$(BUF) lint
	@mkdir -p $(dir $(PROTO_IMAGE))
	$(BUF) build -o $(PROTO_IMAGE)
	cd $(PROTO_DIR) && $(API_LINTER) --config ../.api-linter.yaml \
		--descriptor-set-in ../$(PROTO_IMAGE) --set-exit-status --output-format summary \
		$(PROTO_FILES)

## proto-check: fail when the committed proto, gen/ or api/ differ from a fresh generation
proto-check: proto
	@git diff --exit-code -- $(PROTO_DIR) $(GENERATED)
	@test -z "$$(git status --porcelain -- $(PROTO_DIR) $(GENERATED))" || \
		{ git status --porcelain -- $(PROTO_DIR) $(GENERATED); echo "uncommitted generated files"; exit 1; }

## proto-breaking: breaking changes against BREAKING_AGAINST; skipped while it has no proto
proto-breaking:
	@if git ls-tree -r --name-only $(BREAKING_AGAINST) -- $(PROTO_DIR) | grep -q '\.proto$$'; then \
		$(BUF) breaking --against '.git#ref=$(BREAKING_AGAINST)'; \
	else \
		echo "$(BREAKING_AGAINST) has no proto files, nothing to compare"; \
	fi

## lint: Go and proto linters
lint: proto-lint
	golangci-lint run ./...

## test: Go tests with the race detector
test:
	go test -race ./...
