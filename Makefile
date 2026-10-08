IMAGE   ?= paprika-adapter:local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ENVFILE ?= $(HOME)/.config/paprika-adapter/paprika.env
RUN      = docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges \
           --env-file $(ENVFILE) -v paprika-data:/data

.PHONY: build test integration tools image linux-arm64 sync verify export demo

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/paprika-adapter ./cmd/paprika-adapter

test:
	go vet ./...
	go test -race -count=1 ./...

# Talks to the real account in the environment. See docs/RUNBOOK.md before running.
integration:
	go test -tags integration -run Live -count=1 -v ./internal/paprika/

# Regenerate docs/tools.json after changing a tool.
tools:
	go test ./cmd/paprika-adapter -run TestPublishedToolSchemasAreCurrent -update

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/paprika-adapter-linux-arm64 ./cmd/paprika-adapter

sync:
	$(RUN) $(IMAGE) sync

verify:
	$(RUN) $(IMAGE) verify

export:
	$(RUN) $(IMAGE) export - > paprika-export-$$(date -u +%Y%m%dT%H%M%SZ).tar

# End-to-end flow against the built-in fake Paprika.
demo: build
	go run ./cmd/demo -adapter bin/paprika-adapter -yes
