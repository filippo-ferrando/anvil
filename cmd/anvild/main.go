// Command anvild is anvil's daemon: it owns instance lifecycle, the image
// vault, and the registry, and exposes them over gRPC on a unix socket.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/config"
	"github.com/anvil-project/anvil/internal/container"
	"github.com/anvil-project/anvil/internal/container/docker"
	"github.com/anvil-project/anvil/internal/daemon"
	"github.com/anvil-project/anvil/internal/export"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/intent"
	"github.com/anvil-project/anvil/internal/intent/dns"
	"github.com/anvil-project/anvil/internal/migrate"
	"github.com/anvil-project/anvil/internal/store"
	"github.com/anvil-project/anvil/internal/vm/image"
)

// VMBackend is what main needs from this platform's VM backend beyond the
// plain instance.Backend contract: image-catalog listing, migration export, and import support.
type VMBackend interface {
	instance.Backend
	daemon.VMCatalog
	migrate.Exporter
	export.VMImporter
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("anvild: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for _, dir := range []string{config.RunDir, config.StateDir, config.CacheDir, config.PreparedImageDir(), config.CloudInitDir(), config.MigrateStagingDir(), config.ExportStagingDir()} {
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
	vmBackend := newVMBackend(catalog, vault, db)

	// db satisfies container.Source, used to resolve container mirrors at pull time.
	dockerBackend := container.NewDockerBackend(docker.DefaultSocket, db)
	containerBackend := container.NewBackend(dockerBackend)

	// A separate docker.Client instance from the one inside dockerBackend.
	dockerNetworker := container.NewDockerNetworker(docker.NewClient(docker.DefaultSocket))

	mgr := instance.NewManager(db, map[instance.Kind]instance.Backend{
		instance.KindVM:        vmBackend,
		instance.KindContainer: containerBackend,
	})

	if err := mgr.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconciling instance state at startup: %w", err)
	}

	intentMgr := intent.NewManager(db, mgr, dockerNetworker)
	// Best-effort: catches any intent network left behind by a deletion path
	// that didn't clean up, or anything else that could desync the store from actual state.
	if err := intentMgr.ReconcileNetworks(ctx); err != nil {
		log.Printf("anvild: reconciling intent networks: %v", err)
	}

	// Serves "<role>.<intent>.anvil" on each intent's gateway. The periodic
	// refresh retries binds (e.g. a bridge not up yet) and picks up new container addresses.
	dnsServer := dns.NewServer(intentMgr.DNSZones)
	intentMgr.DNS = dnsServer
	go dnsServer.Run(ctx, 30*time.Second)
	migrateMgr := migrate.NewManager(db, mgr, vmBackend, intentMgr)
	exportMgr := export.NewManager(db, mgr, intentMgr, vmBackend)

	socketPath := config.SocketPath()
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket %s: %w", socketPath, err)
	}
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	// Grants access to non-root CLI/TUI users via the "anvil" group; best-effort.
	_ = os.Chmod(socketPath, 0o660)

	grpcServer := grpc.NewServer()
	anvilv1.RegisterInstanceServiceServer(grpcServer, daemon.NewServer(mgr, intentMgr))
	anvilv1.RegisterCloudInitServiceServer(grpcServer, daemon.NewCloudInitServer(db))
	anvilv1.RegisterMirrorServiceServer(grpcServer, daemon.NewMirrorServer(db))
	anvilv1.RegisterImageServiceServer(grpcServer, daemon.NewImageServer(db, vault, vmBackend, containerBackend))
	anvilv1.RegisterIntentServiceServer(grpcServer, daemon.NewIntentServer(intentMgr))
	anvilv1.RegisterHostServiceServer(grpcServer, daemon.NewHostServer(db, migrateMgr))
	anvilv1.RegisterMigrateServiceServer(grpcServer, daemon.NewMigrateServer(migrateMgr))
	anvilv1.RegisterExportServiceServer(grpcServer, daemon.NewExportServer(exportMgr))
	anvilv1.RegisterSnapshotServiceServer(grpcServer, daemon.NewSnapshotServer(mgr))

	errCh := make(chan error, 1)
	go func() { errCh <- grpcServer.Serve(lis) }()

	log.Printf("anvild: listening on %s", socketPath)
	select {
	case <-ctx.Done():
		log.Print("anvild: shutting down")
		// GracefulStop waits for in-flight RPCs to finish; fall back to a
		// hard Stop if that takes too long.
		stopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			log.Print("anvild: graceful shutdown timed out (an RPC, e.g. `logs --follow`, was still in flight), forcing stop")
			grpcServer.Stop()
		}
		return nil
	case err := <-errCh:
		return err
	}
}
