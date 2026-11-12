# Every check runs with `make ci`; the hosted workflow only calls make targets.

GO_IMAGE     := golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9
LINT_VERSION := 2.15.0
VULN_VERSION := v1.9.0
IMAGE        := stargaite:dev
PLATFORMS    := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

# Tools are kept per host system and per version: the container and the host share bin/.
BIN    := bin/$(shell uname -sm | tr ' ' '-')-$(LINT_VERSION)-$(VULN_VERSION)
SHA256 := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo shasum -a 256)

define LINT_SHA256
767b899a28285e88ded2823d5dcc4bc5ef681173eb2d80f029a7b17ee4ece3b8  golangci-lint-2.15.0-darwin-amd64.tar.gz
d1e409481a8ca846c4bc97e781eb5cb1bd5c23353a0b3977a1cbda1b1b6447d5  golangci-lint-2.15.0-darwin-arm64.tar.gz
9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea  golangci-lint-2.15.0-linux-amd64.tar.gz
b4d8f17068b35ee6060cc2f69a16fb48740d706fbba81661c318649c114a6998  golangci-lint-2.15.0-linux-arm64.tar.gz
endef
export LINT_SHA256

.PHONY: ci check tools ci-container image image-test

# Tools and checks run with an empty environment, so none can lean on an ambient credential.
ci:
	@GOTOOLCHAIN=local go list -m >/dev/null 2>&1 || { echo "make ci needs Go 1.27 or later; 'make ci-container' runs it inside the golang image"; exit 1; }
	env -i PATH="$(PATH)" HOME="$(HOME)" $(MAKE) tools check

check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...
	go mod tidy -diff
	$(BIN)/golangci-lint run
	$(BIN)/govulncheck ./...
	@for p in $(PLATFORMS); do \
		echo "build $$p"; \
		GOOS=$${p%/*} GOARCH=$${p#*/} CGO_ENABLED=0 go build -trimpath -buildvcs=false ./... || exit 1; \
	done
	go test -race ./...

tools: $(BIN)/golangci-lint $(BIN)/govulncheck

# The checksum line is looked up before the download: no line for this system, no download.
$(BIN)/golangci-lint:
	@mkdir -p $(BIN)
	f=golangci-lint-$(LINT_VERSION)-$$(go env GOOS)-$$(go env GOARCH).tar.gz && cd $(BIN) && \
	sum=$$(printf '%s\n' "$$LINT_SHA256" | grep " $$f$$") && \
	curl -fsSL -o $$f https://github.com/golangci/golangci-lint/releases/download/v$(LINT_VERSION)/$$f && \
	echo "$$sum" | $(SHA256) -c - && \
	tar -xzf $$f --strip-components=1 $${f%.tar.gz}/golangci-lint && rm $$f

$(BIN)/govulncheck:
	CGO_ENABLED=0 GOBIN="$(CURDIR)/$(BIN)" go install golang.org/x/vuln/cmd/govulncheck@$(VULN_VERSION)

# The same checks for a machine without Go 1.27 or a C compiler (the race detector needs one).
ci-container:
	docker run --rm --user "$$(id -u):$$(id -g)" -e HOME=/tmp -v "$(CURDIR)":/src -w /src $(GO_IMAGE) make ci

# The version reaches docker through the environment, never through the command text.
image: export VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
image:
	docker build --build-arg VERSION -t $(IMAGE) .
	test "$$(docker run --rm $(IMAGE) version)" = "$$VERSION"

# A read-only root and a volume on /data: the ready line proves that the non-root
# user writes the data directory, and SIGTERM must stop the proxy with exit 0.
image-test: image
	@id=$$(docker run -d --read-only -v /data $(IMAGE)) || exit 1; \
	for i in $$(seq 40); do docker logs $$id 2>&1 | grep -q '"msg":"ready"' && break; sleep 0.25; done; \
	docker stop -t 35 $$id >/dev/null; \
	code=$$(docker inspect -f '{{.State.ExitCode}}' $$id); logs=$$(docker logs $$id 2>&1); \
	docker container rm --volumes $$id >/dev/null; echo "$$logs"; \
	echo "$$logs" | grep -q '"msg":"ready","data_dir":"/data"' && echo "$$logs" | grep -q '"msg":"stopped"' && test "$$code" = 0
