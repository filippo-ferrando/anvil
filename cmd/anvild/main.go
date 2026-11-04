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
	"path/filepath"
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
	"git.fferrando.cc/rdfilippo/anvil/internal/pipeline"
	"git.fferrando.cc/rdfilippo/anvil/internal/runner"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/version"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// VMBackend is what main needs from this platform's VM backend beyond the
// plain instance.Backend contract: image-catalog listing and replacement, migration export, and import support.
type VMBackend interface {
	instance.Backend
	daemon.VMCatalog
	SetCatalog(*image.Catalog)
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

	if err := makeDirs(); err != nil {
		return err
	}

	db, err := store.Open(config.DBPath())
	if err != nil {
		return err
	}
	defer db.Close()

	catalogUpdater, err := newCatalogUpdater()
	if err != nil {
		return err
	}
	catalog, err := catalogUpdater.Initial()
	if err != nil {
		return fmt.Errorf("loading image catalog: %w", err)
	}
	vault := image.NewVault(config.PreparedImageDir())
	vault.RawDir = config.FirecrackerImageDir()
	vault.Keep = instance.KeepUntilCreated
	vmBackend := newVMBackend(catalog, vault, db)
	catalogUpdater.Apply = vmBackend.SetCatalog
	go catalogUpdater.Run(ctx)

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

	intentMgr.DNS = startDNS(ctx, intentMgr)
	startDiscovery(ctx)
	startMetrics(ctx, mgr)

	migrateMgr := migrate.NewManager(db, mgr, vmBackend, intentMgr)
	exportMgr := export.NewManager(db, mgr, intentMgr, vmBackend)

	socketPath := config.SocketPath()
	lis, err := listenSocket(socketPath)
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer(daemon.ActorInterceptors()...)
	instanceServer := daemon.NewServer(mgr, intentMgr, vault)
	anvilv1.RegisterInstanceServiceServer(grpcServer, instanceServer)
	anvilv1.RegisterCloudInitServiceServer(grpcServer, daemon.NewCloudInitServer(db))
	anvilv1.RegisterManifestServiceServer(grpcServer, daemon.NewManifestServer(db))
	anvilv1.RegisterMirrorServiceServer(grpcServer, daemon.NewMirrorServer(db))
	imageServer := daemon.NewImageServer(db, vault, vmBackend, containerBackend,
		&image.FCCache{Dir: config.FirecrackerImageDir(), Vault: vault})
	imageServer.CatalogUpdater = catalogUpdater
	anvilv1.RegisterImageServiceServer(grpcServer, imageServer)
	intentServer := daemon.NewIntentServer(intentMgr, mgr, instanceServer)
	anvilv1.RegisterIntentServiceServer(grpcServer, intentServer)
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
	// Runners poll their forges in the background; jobs of the previous anvild went with it.
	runnerMgr := runner.NewManager(db, ephemeralMgr)
	runnerMgr.MarkInterrupted()
	if err := runnerMgr.Start(ctx); err != nil {
		log.Printf("anvild: starting runners: %v", err)
	}
	anvilv1.RegisterRunnerServiceServer(grpcServer, daemon.NewRunnerServer(ctx, runnerMgr))
	// Pipeline runs of the previous anvild lost their clones; watches poll in the background.
	pipelineExec := pipeline.NewExecutor(db, ephemeralMgr, config.PipelineRunsDir(), daemon.PipelineApply(intentServer.Applier))
	pipelineExec.MarkInterrupted()
	pipelineWatcher := pipeline.NewWatcher(pipelineExec, filepath.Join(config.CacheDir, "pipeline-clones"))
	pipelineWatcher.Start(ctx)
	anvilv1.RegisterPipelineServiceServer(grpcServer, daemon.NewPipelineServer(ctx, pipelineExec, pipelineWatcher, filepath.Join(config.CacheDir, "pipeline-uploads")))

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
		if !waitTimeout(grpcServer.GracefulStop, 10*time.Second) {
			log.Print("anvild: graceful shutdown timed out (an RPC, e.g. `logs --follow`, was still in flight), forcing stop")
			grpcServer.Stop()
		}
		drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Runner jobs and watched pipelines stop with ctx; their clones go before the pools.
		drained := waitContext(drainCtx, func() {
			runnerMgr.Wait()
			pipelineWatcher.Wait()
			ephemeralMgr.WaitDeletes()
		})
		if !drained {
			log.Print("anvild: timed out waiting for runner jobs, pipelines and clone deletes")
		}
		ephemeralMgr.Shutdown(drainCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// newCatalogUpdater keeps the image catalog in line with the official one.
// ANVIL_CATALOG_URL changes where it's fetched from, ANVIL_CATALOG_UPDATE=off stops automatic checks.
func newCatalogUpdater() (*image.CatalogUpdater, error) {
	url := os.Getenv("ANVIL_CATALOG_URL")
	if url == "" {
		url = image.DefaultCatalogURL
	}
	u, err := image.NewCatalogUpdater(url, config.CatalogCachePath())
	if err != nil {
		return nil, err
	}
	if os.Getenv("ANVIL_CATALOG_UPDATE") == "off" {
		u.Interval = 0
	}
	return u, nil
}

// makeDirs creates the run, state and cache directories anvild writes to.
func makeDirs() error {
	dirs := []string{
		config.RunDir, config.StateDir, config.CacheDir,
		config.PreparedImageDir(), config.MigrateStagingDir(), config.ExportStagingDir(),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating %s: %w (run as root, or as the \"anvil\" system user once packaged)", dir, err)
		}
	}
	return nil
}

// listenSocket replaces any stale socket at path and listens on it.
func listenSocket(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	// Grants access to non-root CLI/TUI users via the "anvil" group; best-effort.
	_ = os.Chmod(path, 0o660)
	return lis, nil
}

// startDNS serves "<role>.<intent>.anvil" on each intent's gateway. The periodic
// refresh retries binds (e.g. a bridge not up yet) and picks up new container addresses.
func startDNS(ctx context.Context, intents *intent.Manager) *dns.Server {
	server := dns.NewServer(func(ctx context.Context) ([]dns.Zone, error) {
		zones, err := intents.DNSZones(ctx)
		return append(zones, platformDNSZones()...), err
	})
	platformDNSHook(func() {
		if err := server.Refresh(ctx); err != nil {
			log.Printf("dns: %v", err)
		}
	})
	go server.Run(ctx, 30*time.Second)
	return server
}

// startDiscovery announces this host to `anvil host discover` on the same network.
// It grants no access by itself, and ANVIL_MDNS=off turns it off.
func startDiscovery(ctx context.Context) {
	if os.Getenv("ANVIL_MDNS") == "off" {
		return
	}
	go func() {
		if err := discovery.Serve(ctx, "", 0); err != nil {
			log.Printf("anvild: mDNS discovery: %v", err)
		}
	}()
}

// startMetrics serves the opt-in Prometheus endpoint:
// ANVIL_METRICS="127.0.0.1:9469" or "unix:/run/anvil/metrics.sock".
func startMetrics(ctx context.Context, mgr *instance.Manager) {
	addr := os.Getenv("ANVIL_METRICS")
	if addr == "" {
		return
	}
	go func() {
		if err := daemon.ServeMetrics(ctx, addr, mgr); err != nil {
			log.Printf("anvild: metrics: %v", err)
		}
	}()
}

// waitTimeout runs fn and reports whether it returned within d.
func waitTimeout(fn func(), d time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return waitContext(ctx, fn)
}

// waitContext runs fn and reports whether it returned before ctx was done.
func waitContext(ctx context.Context, fn func()) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
