.PHONY: proto build test vet fmt clean

# Regenerates api/gen/anvil/v1 from api/proto/anvil/v1/anvil.proto.
# Requires protoc (present on Arch via the `protobuf` package) plus the Go
# plugins below — this sandbox's dev session has no network access to `go
# install` them, so this target is untested here; run it once on a machine
# with network access before `go build` will succeed for anything that
# imports api/gen/anvil/v1 (internal/daemon, pkg/client, internal/cli,
# internal/tui).
proto:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	protoc \
		--go_out=api/gen --go_opt=paths=source_relative \
		--go-grpc_out=api/gen --go-grpc_opt=paths=source_relative \
		--proto_path=api/proto \
		api/proto/anvil/v1/anvil.proto

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
