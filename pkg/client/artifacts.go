package client

import (
	"context"
	"errors"
	"io"
	"os"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
)

// DownloadArtifacts extracts a pipeline run's collected files into dir, as <job>/<distro>/...
func DownloadArtifacts(ctx context.Context, pc anvilv1.PipelineServiceClient, id, dir string) error {
	stream, err := pc.Artifacts(ctx, &anvilv1.PipelineArtifactsRequest{Id: id})
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := os.MkdirAll(dir, 0o755)
		if err == nil {
			err = guestexec.Extract(pr, dir)
		}
		pr.CloseWithError(err)
		_, _ = io.Copy(io.Discard, pr)
		done <- err
	}()
	for {
		ch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			pw.CloseWithError(err)
			<-done
			return err
		}
		if _, err := pw.Write(ch.GetData()); err != nil {
			break
		}
	}
	pw.Close()
	return <-done
}
