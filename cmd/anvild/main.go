// Command anvild is anvil's daemon: it owns all business logic (instance
// lifecycle, image vault, registry) and exposes it over gRPC on a unix
// socket. anvil (the CLI) and, later, anvil tui are both thin clients of
// this API — see pkg/client.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/config"
	"github.com/anvil-project/anvil/internal/container"
	"github.com/anvil-project/anvil/internal/container/docker"
	"github.com/anvil-project/anvil/internal/daemon"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/intent"
	"github.com/anvil-project/anvil/internal/migrate"
	"github.com/anvil-project/anvil/internal/store"
	"github.com/anvil-project/anvil/internal/vm"
	"github.com/anvil-project/anvil/internal/vm/image"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("anvild: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for _, dir := range []string{config.RunDir, config.StateDir, config.CacheDir, config.PreparedImageDir(), config.CloudInitDir(), config.MigrateStagingDir()} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating %s: %w (run as root, or as the \"anvil\" system user once packaged)", dir, err)
		}
	}

	db, err := store.Open(config.DBPath())
	if err != nil {
		return err
	}
	defer db.Close()

	catalog, err := image.LoadEmbedded()
	if err != nil {
		return fmt.Errorf("loading image catalog: %w", err)
	}
	vault := image.NewVault(config.PreparedImageDir())
	vmBackend := vm.NewBackend(catalog, vault, db)

	// The docker.Client is just an HTTP client wrapper — constructing it
	// doesn't connect to anything, so this doesn't fail (and doesn't need
	// to be conditional on Docker actually being installed/running) even
	// on a host with no Docker at all; that only surfaces as a normal
	// per-request error the first time someone actually tries
	// `anvil launch --kind container`. Podman is deferred (filippo doesn't
	// have it on this machine to test against), hence container.Backend.Podman
	// staying nil. db satisfies container.Source (just ListMirrors), used to
	// resolve `--kind container` mirrors at pull time.
	dockerBackend := container.NewDockerBackend(docker.DefaultSocket, db)
	containerBackend := container.NewBackend(dockerBackend)

	// A second docker.Client instance, not the one inside dockerBackend —
	// harmless, it's just an HTTP client wrapper with no connection state
	// of its own, and this keeps DockerNetworker's construction
	// independent of DockerBackend's.
	dockerNetworker := container.NewDockerNetworker(docker.NewClient(docker.DefaultSocket))

	mgr := instance.NewManager(db, map[instance.Kind]instance.Backend{
		instance.KindVM:        vmBackend,
		instance.KindContainer: containerBackend,
	})

	if err := mgr.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconciling instance state at startup: %w", err)
	}

	intentMgr := intent.NewManager(db, mgr, dockerNetworker)
	migrateMgr := migrate.NewManager(db, mgr, vmBackend)

	socketPath := config.SocketPath()
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket %s: %w", socketPath, err)
	}
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	// 0660 + a dedicated "anvil" group (set up by packaging's sysusers.d
	// rule) is how non-root CLI/TUI users get access — see the plan's
	// "Arch Linux packaging" section. Best-effort here since running this
	// binary directly in dev (not via the systemd unit) may not have an
	// "anvil" group to chown to.
	_ = os.Chmod(socketPath, 0o660)

	grpcServer := grpc.NewServer()
	anvilv1.RegisterInstanceServiceServer(grpcServer, daemon.NewServer(mgr, intentMgr))
	anvilv1.RegisterCloudInitServiceServer(grpcServer, daemon.NewCloudInitServer(db))
	anvilv1.RegisterMirrorServiceServer(grpcServer, daemon.NewMirrorServer(db))
	anvilv1.RegisterImageServiceServer(grpcServer, daemon.NewImageServer(db, vault, vmBackend))
	anvilv1.RegisterIntentServiceServer(grpcServer, daemon.NewIntentServer(intentMgr))
	anvilv1.RegisterHostServiceServer(grpcServer, daemon.NewHostServer(db, migrateMgr))
	anvilv1.RegisterMigrateServiceServer(grpcServer, daemon.NewMigrateServer(migrateMgr))

	errCh := make(chan error, 1)
	go func() { errCh <- grpcServer.Serve(lis) }()

	log.Printf("anvild: listening on %s", socketPath)
	select {
	case <-ctx.Done():
		log.Print("anvild: shutting down")
		grpcServer.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}
