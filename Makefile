# Every check runs with `make ci`; the hosted workflow only calls make targets.

GO_IMAGE     := golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9
LINT_VERSION := 2.15.0
VULN_VERSION := v1.9.0
SHELLCHECK_VERSION := v0.11.0
JQ_VERSION   := 1.8.2
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

define SHELLCHECK_SHA256
c2c15e08df0e8fbc374c335b230a7ee958c313fa5714817a59aa59f1aa594f51  shellcheck-v0.11.0.darwin.x86_64.tar.gz
339b930feb1ea764467013cc1f72d09cd6b869ebf1013296ba9055ab2ffbd26f  shellcheck-v0.11.0.darwin.aarch64.tar.gz
b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6  shellcheck-v0.11.0.linux.x86_64.tar.gz
68a8133197a50beb8803f8d42f9908d1af1c5540d4bb05fdfca8c1fa47decefc  shellcheck-v0.11.0.linux.aarch64.tar.gz
endef
export SHELLCHECK_SHA256

define JQ_SHA256
e94b266e3c26690550006abe63152b782280f4e14374accdf04cbde844f00bc0  jq-macos-amd64
2d75340ba57a4b4b4c8708a21c2dc8e958a48aaa8bba13b27f77f6e4c0eca07e  jq-macos-arm64
b1c22172dd303f3be49e935aa56aa48a8b7a46e0bc838b4997d3bb451495870f  jq-linux-amd64
8b85c817833814ddca00a144c33705546355afccf0cf39b188f3cdb48b852309  jq-linux-arm64
endef
export JQ_SHA256

.PHONY: ci check tools ci-container image image-test setup-test fuzz bench bench-container

# Tools and checks run with an empty environment, so none can lean on an ambient credential.
ci:
	@GOTOOLCHAIN=local go list -m >/dev/null 2>&1 || { echo "make ci needs Go 1.27.4 or later; 'make ci-container' runs it inside the golang image"; exit 1; }
	env -i PATH="$(PATH)" HOME="$(HOME)" $(MAKE) tools check

check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...
	go mod tidy -diff
	@for p in $(PLATFORMS); do \
		GOOS=$${p%/*} GOARCH=$${p#*/} CGO_ENABLED=0 go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}' ./cmd/stargaite || echo "go list failed for $$p"; \
	done | sort -u | diff -u cmd/stargaite/modules.txt - || { echo "the proxy's module set changed (docs/design/phase-7.md section 5): - listed, + built"; exit 1; }
	$(BIN)/golangci-lint run
	$(BIN)/govulncheck ./...
	$(BIN)/shellcheck --shell=bash setup.sh tests/setup/e2e.sh
	@for p in $(PLATFORMS); do \
		echo "build $$p"; \
		GOOS=$${p%/*} GOARCH=$${p#*/} CGO_ENABLED=0 go build -trimpath -buildvcs=false ./... || exit 1; \
	done
	go test -race ./...

# shellcheck lints setup.sh; jq is for the tests of tests/setup and make setup-test.
tools: $(BIN)/golangci-lint $(BIN)/govulncheck $(BIN)/shellcheck $(BIN)/jq

# Outside make ci, which replays the committed seeds: every Fuzz target of the
# repository runs for FUZZTIME, once per package and target. -fuzz is a regular
# expression, hence the anchors (FuzzStream would also match FuzzChatStream).
# A finding is written under the package's testdata/fuzz: commit it as a seed
# with the fix.
FUZZTIME ?= 60s
fuzz:
	@grep -rHo --include='*_test.go' '^func Fuzz[A-Za-z0-9_]*' . | sort -u | while IFS=: read -r file fn; do \
		dir=./$${file#./}; dir=$${dir%/*}; name=$${fn#func }; \
		echo "fuzz $$dir $$name"; \
		go test -run '^$$' -fuzz "^$$name\$$" -fuzztime $(FUZZTIME) "$$dir" || exit 1; \
	done

# Outside make ci, the benchmark of kick-off section 18 (AC-52; Linux): the
# binary and the mock are built into BENCHDIR, and tests/benchmark runs both
# as processes, the proxy with GOMAXPROCS=1, pinned to CPU 0 when taskset is
# present. STARGAITE_BENCH_OUT=<file> writes the JSON document there instead
# of standard output. A missed target fails unless docs/benchmark.md lists it
# as an accepted deviation; review that file after each run.
BENCHDIR ?= $(or $(TMPDIR),/tmp)/stargaite-bench
bench:
	@mkdir -p "$(BENCHDIR)"
	CGO_ENABLED=0 go build -trimpath -o "$(BENCHDIR)/stargaite" ./cmd/stargaite
	CGO_ENABLED=0 go build -trimpath -o "$(BENCHDIR)/mockprovider" ./tests/mockprovider/cmd/mockprovider
	STARGAITE_BENCH=1 STARGAITE_BENCH_DIR="$(BENCHDIR)" STARGAITE_BENCH_OUT="$(abspath $(STARGAITE_BENCH_OUT))" \
		go test -count=1 -v -run '^TestBenchmark$$' -timeout 60m ./tests/benchmark

# The same inside the Go image; a relative STARGAITE_BENCH_OUT lands in the
# clone. No docker inside it, so the image size is not measured there.
bench-container:
	docker run --rm --user "$$(id -u):$$(id -g)" -e HOME=/tmp -e STARGAITE_BENCH_OUT -v "$(CURDIR)":/src -w /src $(GO_IMAGE) make bench

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

# These two name the system with uname, not go env: make setup-test runs on a
# host without Go.
$(BIN)/shellcheck:
	@mkdir -p $(BIN)
	f=shellcheck-$(SHELLCHECK_VERSION).$$(uname -s | tr A-Z a-z).$$(uname -m | sed s/arm64/aarch64/).tar.gz && cd $(BIN) && \
	sum=$$(printf '%s\n' "$$SHELLCHECK_SHA256" | grep " $$f$$") && \
	curl -fsSL -o $$f https://github.com/koalaman/shellcheck/releases/download/$(SHELLCHECK_VERSION)/$$f && \
	echo "$$sum" | $(SHA256) -c - && \
	tar -xzf $$f --strip-components=1 shellcheck-$(SHELLCHECK_VERSION)/shellcheck && rm $$f

$(BIN)/jq:
	@mkdir -p $(BIN)
	f=jq-$$(uname -s | tr A-Z a-z | sed s/darwin/macos/)-$$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') && cd $(BIN) && \
	sum=$$(printf '%s\n' "$$JQ_SHA256" | grep " $$f$$") && \
	curl -fsSL -o $$f https://github.com/jqlang/jq/releases/download/jq-$(JQ_VERSION)/$$f && \
	echo "$$sum" | $(SHA256) -c - && \
	chmod +x $$f && mv $$f jq

# The same checks for a machine without Go 1.27.4 or a C compiler (the race detector needs one).
ci-container:
	docker run --rm --user "$$(id -u):$$(id -g)" -e HOME=/tmp -v "$(CURDIR)":/src -w /src $(GO_IMAGE) make ci

# The version reaches docker through the environment, never through the command text.
image: export VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
image:
	docker build --build-arg VERSION -t $(IMAGE) .
	test "$$(docker run --rm $(IMAGE) version)" = "$$VERSION"

# A read-only root and a volume on /data: the ready line proves that the non-root
# user writes the data directory; the CLI inside the container proves that the
# admin key was bootstrapped there and the admin API answers; and SIGTERM must
# stop the proxy with exit 0.
image-test: image
	@id=$$(docker run -d --read-only -v /data $(IMAGE)) || exit 1; \
	for i in $$(seq 40); do docker logs $$id 2>&1 | grep -q '"msg":"ready"' && break; sleep 0.25; done; \
	shown=$$(docker exec $$id /stargaite settings show --json); shown_code=$$?; \
	docker stop -t 35 $$id >/dev/null; \
	code=$$(docker inspect -f '{{.State.ExitCode}}' $$id); logs=$$(docker logs $$id 2>&1); \
	docker container rm --volumes $$id >/dev/null; echo "$$logs"; echo "settings show: exit $$shown_code: $$shown"; \
	echo "$$logs" | grep -q '"msg":"ready","data_dir":"/data"' && echo "$$logs" | grep -q '"msg":"stopped"' && test "$$code" = 0 && test "$$shown_code" = 0

# Outside make ci, as image-test (PUB-5): setup.sh end to end on a Linux host
# with Docker, from a temporary copy of the working tree, against the mock
# provider; it runs beside a service on port 4000 and removes only the
# project, volume, images and container it created.
setup-test: $(BIN)/jq
	PATH="$(CURDIR)/$(BIN):$$PATH" GO_IMAGE=$(GO_IMAGE) bash tests/setup/e2e.sh
