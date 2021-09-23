package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/filecoin-project/indexer-reference-provider/internal/suppliers"
	"github.com/ipld/go-car/v2"

	"github.com/filecoin-project/indexer-reference-provider/config"
	"github.com/filecoin-project/indexer-reference-provider/core/engine"
	adminserver "github.com/filecoin-project/indexer-reference-provider/server/admin/http"
	p2pserver "github.com/filecoin-project/indexer-reference-provider/server/provider/libp2p"
	leveldb "github.com/ipfs/go-ds-leveldb"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	"github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/urfave/cli/v2"
)

var log = logging.Logger("command/reference-provider")

var (
	ErrDaemonStart = errors.New("daemon did not start correctly")
	ErrDaemonStop  = errors.New("daemon did not stop gracefully")
)

const (
	// shutdownTimeout is the duration that a graceful shutdown has to complete
	shutdownTimeout = 5 * time.Second
)

var DaemonCmd = &cli.Command{
	Name:   "daemon",
	Usage:  "Starts a reference provider",
	Flags:  daemonFlags,
	Action: daemonCommand,
}

func daemonCommand(cctx *cli.Context) error {
	cfg, err := config.Load("")
	if err != nil {
		if err == config.ErrNotInitialized {
			fmt.Fprintln(os.Stderr, "reference provider is not initialized")
			fmt.Fprintln(os.Stderr, "To initialize, run using the \"init\" command")
			os.Exit(1)
		}
		return fmt.Errorf("cannot load config file: %w", err)
	}

	// Initialize libp2p host
	ctx, cancelp2p := context.WithCancel(cctx.Context)
	defer cancelp2p()

	privKey, err := cfg.Identity.DecodePrivateKey("")
	if err != nil {
		return err
	}

	p2pmaddr, err := multiaddr.NewMultiaddr(cfg.ProviderServer.ListenMultiaddr)
	if err != nil {
		return fmt.Errorf("bad p2p address in config %s: %s", cfg.ProviderServer.ListenMultiaddr, err)
	}
	h, err := libp2p.New(ctx,
		// Use the keypair generated during init
		libp2p.Identity(privKey),
		// Listen to p2p addr specified in config
		libp2p.ListenAddrs(p2pmaddr),
	)
	if err != nil {
		return err
	}
	log.Infow("libp2p host initialized", "host_id", h.ID(), "multiaddr", p2pmaddr)

	// Initialize datastore
	if cfg.Datastore.Type != "levelds" {
		return fmt.Errorf("only levelds datastore type supported, %q not supported", cfg.Datastore.Type)
	}
	dataStorePath, err := config.Path("", cfg.Datastore.Dir)
	if err != nil {
		return err
	}
	err = checkWritable(dataStorePath)
	if err != nil {
		return err
	}
	ds, err := leveldb.NewDatastore(dataStorePath, nil)
	if err != nil {
		return err
	}
	// Starting provider core
	eng, err := engine.New(ctx, privKey, h, ds, cfg.Ingest.PubSubTopic, cfg.ProviderServer.RetrievalMultiaddrs)
	if err != nil {
		return err
	}

	// Instantiate CAR supplier and register it as a callback onto the engine.
	cs := suppliers.NewCarSupplier(eng, ds, car.ZeroLengthSectionAsEOF(carZeroLengthAsEOFFlagValue))

	// Starting provider p2p server
	p2pserver.New(ctx, h, eng)
	log.Infow("libp2p servers initialized", "host_id", h.ID())

	maddr, err := multiaddr.NewMultiaddr(cfg.AdminServer.ListenMultiaddr)
	if err != nil {
		return fmt.Errorf("bad admin address in config %s: %s", cfg.AdminServer.ListenMultiaddr, err)
	}
	adminAddr, err := manet.ToNetAddr(maddr)
	if err != nil {
		return err
	}
	adminSvr, err := adminserver.New(
		adminAddr.String(),
		h,
		eng,
		cs,
		adminserver.ReadTimeout(cfg.AdminServer.ReadTimeout),
		adminserver.WriteTimeout(cfg.AdminServer.WriteTimeout))
	if err != nil {
		return err
	}
	log.Infow("admin server initialized", "address", adminAddr)

	errChan := make(chan error, 1)
	go func() {
		errChan <- adminSvr.Start()
	}()
	var finalErr error
	// Keep process running.
	select {
	case <-cctx.Done():
	case err = <-errChan:
		log.Errorw("Failed to start server", "err", err)
		finalErr = ErrDaemonStart
	}

	log.Infow("Shutting down daemon")

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	go func() {
		// Wait for context to be canceled. If timeout, then exit with error.
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Println("Timed out on shutdown, terminating...")
			os.Exit(-1)
		}
	}()

	if err = eng.Close(ctx); err != nil {
		log.Errorw("Error closing provider core", "err", err)
		finalErr = ErrDaemonStop
	}

	// cancel libp2p server
	cancelp2p()

	if err = adminSvr.Shutdown(ctx); err != nil {
		log.Errorw("Error shutting down admin server", "err", err)
		finalErr = ErrDaemonStop
	}
	log.Infow("node stopped")
	return finalErr
}
