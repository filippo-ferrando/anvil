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

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/container"
	"git.fferrando.cc/rdfilippo/anvil/internal/container/docker"
	"git.fferrando.cc/rdfilippo/anvil/internal/daemon"
	"git.fferrando.cc/rdfilippo/anvil/internal/discovery"
	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/export"
	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/dns"
	"git.fferrando.cc/rdfilippo/anvil/internal/migrate"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/version"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
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
	// Run by anvil-devhelper@.service as root, one connection per call: see internal/hostdev.
	if len(os.Args) == 2 && os.Args[1] == "devhelper" {
		if err := hostdev.Serve(); err != nil {
			log.Fatalf("anvild devhelper: %v", err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatalf("anvild: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	v, commit := version.Get()
	log.Printf("anvild: starting %s (%s)", v, commit)

	for _, dir := range []string{config.RunDir, config.StateDir, config.CacheDir, config.PreparedImageDir(), config.MigrateStagingDir(), config.ExportStagingDir()} {
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
	vault.RawDir = config.FirecrackerImageDir()
	vault.Keep = instance.KeepUntilCreated
	vmBackend := newVMBackend(catalog, vault, db)

	// db satisfies container.Source, used to resolve container mirrors at pull time.
	dockerBackend := container.NewDockerBackend(docker.DefaultSocket, db)
	dockerBackend.Registry = db // maps Docker's exit events back to instances
	defer dockerBackend.Close()
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
	dnsServer := dns.NewServer(func(ctx context.Context) ([]dns.Zone, error) {
		zones, err := intentMgr.DNSZones(ctx)
		return append(zones, platformDNSZones()...), err
	})
	intentMgr.DNS = dnsServer
	platformDNSHook(func() {
		if err := dnsServer.Refresh(ctx); err != nil {
			log.Printf("dns: %v", err)
		}
	})
	go dnsServer.Run(ctx, 30*time.Second)
	// Announces this host to `anvil host discover` on the same network. It
	// grants no access by itself, and ANVIL_MDNS=off turns it off.
	if os.Getenv("ANVIL_MDNS") != "off" {
		go func() {
			if err := discovery.Serve(ctx, "", 0); err != nil {
				log.Printf("anvild: mDNS discovery: %v", err)
			}
		}()
	}

	// Opt-in Prometheus endpoint: ANVIL_METRICS="127.0.0.1:9469" or "unix:/run/anvil/metrics.sock".
	if addr := os.Getenv("ANVIL_METRICS"); addr != "" {
		go func() {
			if err := daemon.ServeMetrics(ctx, addr, mgr); err != nil {
				log.Printf("anvild: metrics: %v", err)
			}
		}()
	}

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

	grpcServer := grpc.NewServer(daemon.ActorInterceptors()...)
	instanceServer := daemon.NewServer(mgr, intentMgr, vault)
	anvilv1.RegisterInstanceServiceServer(grpcServer, instanceServer)
	anvilv1.RegisterCloudInitServiceServer(grpcServer, daemon.NewCloudInitServer(db))
	anvilv1.RegisterManifestServiceServer(grpcServer, daemon.NewManifestServer(db))
	anvilv1.RegisterMirrorServiceServer(grpcServer, daemon.NewMirrorServer(db))
	imageServer := daemon.NewImageServer(db, vault, vmBackend, containerBackend,
		&image.FCCache{Dir: config.FirecrackerImageDir(), Vault: vault})
	anvilv1.RegisterImageServiceServer(grpcServer, imageServer)
	anvilv1.RegisterIntentServiceServer(grpcServer, daemon.NewIntentServer(intentMgr, mgr, instanceServer))
	hostServer := daemon.NewHostServer(db, migrateMgr)
	hostServer.Instances = mgr
	if dh, ok := vmBackend.(instance.DeviceHolder); ok {
		hostServer.Devices = dh
	}
	anvilv1.RegisterHostServiceServer(grpcServer, hostServer)
	anvilv1.RegisterMigrateServiceServer(grpcServer, daemon.NewMigrateServer(migrateMgr))
	anvilv1.RegisterExportServiceServer(grpcServer, daemon.NewExportServer(exportMgr))
	anvilv1.RegisterSnapshotServiceServer(grpcServer, daemon.NewSnapshotServer(mgr))
	// Clones and warm builds of the previous anvild died with it; their leftovers go first.
	ephemeralMgr := ephemeral.NewManager(platformEphemeralEngine(), db, config.WarmDir)
	ephemeralMgr.Cleanup(ctx)
	ephemeralMgr.StartPools() // in the background, after the sweep above
	anvilv1.RegisterEphemeralServiceServer(grpcServer, daemon.NewEphemeralServer(ephemeralMgr, config.EphemeralUploadDir()))

	errCh := make(chan error, 1)
	go func() { errCh <- grpcServer.Serve(daemon.CallerListener(lis)) }()

	// Both need the intent networks reconciled above; neither blocks serving requests.
	go mgr.Autostart(ctx)
	go mgr.RunSnapshotScheduler(ctx)

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
