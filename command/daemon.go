package command

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/filecoin-project/indexer-reference-provider/config"
	leveldb "github.com/ipfs/go-ds-leveldb"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	"github.com/urfave/cli/v2"
)

var log = logging.Logger("command/reference-provider")

var (
	ErrDaemonStart = errors.New("daemon did not start correctly")
	ErrDaemonStop  = errors.New("daemon did not stop gracefully")
)

var DaemonCmd = &cli.Command{
	Name:   "daemon",
	Usage:  "Starts a reference provider",
	Flags:  DaemonFlags,
	Action: daemonCommand,
}

func daemonCommand(cctx *cli.Context) error {
	cfg, err := config.Load("")
	if err != nil {
		if err == config.ErrNotInitialized {
			fmt.Fprintln(os.Stderr, "reference provider is not initialized")
			fmt.Fprintln(os.Stderr, "To initialize, run the command: ./indexer-reference-provider init") // TODO adjust `./`; see how we can simplify the message here so that it would make sense regardless of OS or where the binary is located
			os.Exit(1)
		}
		return fmt.Errorf("cannot load config file: %w", err)
	}

	// Initialize libp2p host
	ctx, cancel := context.WithCancel(cctx.Context)
	defer cancel()

	privKey, err := cfg.Identity.DecodePrivateKey("")
	if err != nil {
		return err
	}

	// TODO: Do we want to the libp2p host to listen on any particular
	// addresss and port?
	_, err = libp2p.New(ctx,
		// Use the keypair generated during init
		libp2p.Identity(privKey),
	)
	if err != nil {
		return err
	}

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
	_, err = leveldb.NewDatastore(dataStorePath, nil)
	if err != nil {
		return err
	}

	log.Infow("Reference provider started")

	// Keep process running.
	<-cctx.Done()

	log.Infow("Shutting down daemon")

	go func() {
		// Wait for context to be canceled.  If timeout, then exit with error.
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Println("Timed out on shutdown, terminating...")
			os.Exit(-1)
		}
	}()
	cancel()

	log.Infow("node stopped")
	return nil
}
