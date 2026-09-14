.PHONY: proto tui-deps build test vet fmt clean

# Regenerates api/gen/anvil/v1 from api/proto/anvil/v1/anvil.proto.
proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	protoc \
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

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -s .

clean:
	rm -f anvil anvild
