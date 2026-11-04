.PHONY: proto tui-deps catalog build gui-tools firecracker update-tools man test vet fmt clean

GOBIN := $(shell go env GOPATH)/bin

# Regenerates api/gen/anvil/v1 from api/proto/anvil/v1/anvil.proto.
proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
	protoc \
		--plugin=protoc-gen-go=$(GOBIN)/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=$(GOBIN)/protoc-gen-go-grpc \
		--go_out=api/gen --go_opt=paths=source_relative \
		--go-grpc_out=api/gen --go-grpc_opt=paths=source_relative \
		--proto_path=api/proto \
		api/proto/anvil/v1/anvil.proto

# Updates internal/tui's Bubble Tea dependencies to their latest versions.
tui-deps:
	go get github.com/charmbracelet/bubbletea@latest
	go get github.com/charmbracelet/bubbles@latest
	go get github.com/charmbracelet/lipgloss@latest
	go mod tidy

# The VM image catalog embedded by data/distros, pulled from the anvil/catalog repo.
CATALOG := data/distros/distros_x86_64.json

# Refetches the catalog (CATALOG_REF picks a branch or tag, default main).
catalog:
	scripts/fetch-catalog.sh

$(CATALOG):
	scripts/fetch-catalog.sh

build: $(CATALOG)
	CGO_ENABLED=0 go build ./cmd/anvil/...
	go build ./cmd/anvild/...
	CGO_ENABLED=0 go build ./cmd/anvil-guest/...

# Builds static waypipe and xwayland-satellite for `anvil gui` into build/gui-tools (needs docker).
gui-tools:
	scripts/build-gui-tools.sh build/gui-tools

# Downloads the static firecracker and jailer pinned in scripts/tools.versions into build/firecracker.
firecracker:
	scripts/fetch-firecracker.sh build/firecracker $(or $(ARCH),x86_64)

# Bumps scripts/tools.versions to the latest stable GUI tools and firecracker.
update-tools:
	scripts/update-tools.sh

# Regenerates man/man1 (one page per `anvil` subcommand, via the hidden
# `anvil man` command in internal/cli/commands/man.go) and copies
# docs/man/anvild.8, anvild's hand-written page, into man/man8. Both
# directories are gitignored; the packaging scripts under packaging/
# do their own equivalent of this rather than depending on it, since
# each builds anvil-bin fresh in its own staging area.
man: $(CATALOG)
	rm -rf man
	mkdir -p man/man1 man/man8
	CGO_ENABLED=0 go run ./cmd/anvil man man/man1
	cp docs/man/anvild.8 man/man8/anvild.8

test: $(CATALOG)
	go test ./...

vet: $(CATALOG)
	go vet ./...

fmt:
	gofmt -l -s .

clean:
	rm -f anvil anvild anvil-guest
	rm -rf man build
