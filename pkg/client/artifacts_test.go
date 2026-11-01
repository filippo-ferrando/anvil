package client

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// fakeArtifacts serves one tar as the Artifacts stream, in two chunks.
type fakeArtifacts struct {
	anvilv1.PipelineServiceClient
	data []byte
}

type chunkStream struct {
	grpc.ClientStream
	chunks [][]byte
}

func (s *chunkStream) Recv() (*anvilv1.PipelineChunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return &anvilv1.PipelineChunk{Data: c}, nil
}

func (f fakeArtifacts) Artifacts(context.Context, *anvilv1.PipelineArtifactsRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[anvilv1.PipelineChunk], error) {
	half := len(f.data) / 2
	return &chunkStream{chunks: [][]byte{f.data[:half], f.data[half:]}}, nil
}

func TestDownloadArtifacts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	data := testTar(t, map[string]string{"build/debian-13/app.bin": "binary"}, nil)
	if err := DownloadArtifacts(context.Background(), fakeArtifacts{data: data}, "01RUN", dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "build", "debian-13", "app.bin"))
	if err != nil || string(got) != "binary" {
		t.Errorf("extracted %q, %v", got, err)
	}
}
