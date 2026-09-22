.PHONY: proto tui-deps build man test vet fmt clean

GOBIN := $(shell go env GOPATH)/bin

# Regenerates api/gen/anvil/v1 from api/proto/anvil/v1/anvil.proto.
proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
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

build:
	CGO_ENABLED=0 go build ./cmd/anvil/...
	go build ./cmd/anvild/...

# Regenerates man/man1 (one page per `anvil` subcommand, via the hidden
# `anvil man` command in internal/cli/commands/man.go) and copies
# docs/man/anvild.8, anvild's hand-written page, into man/man8. Both
# directories are gitignored; the packaging scripts under packaging/
# do their own equivalent of this rather than depending on it, since
# each builds anvil-bin fresh in its own staging area.
man:
	rm -rf man
	mkdir -p man/man1 man/man8
	CGO_ENABLED=0 go run ./cmd/anvil man man/man1
	cp docs/man/anvild.8 man/man8/anvild.8

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -s .

clean:
	rm -f anvil anvild
	rm -rf man
