package p2pserver

import (
	"context"

	"github.com/filecoin-project/index-provider/engine"
	"github.com/filecoin-project/index-provider/libp2pserver"
	"github.com/libp2p/go-libp2p-core/host"
)

// New creates a new libp2p server
func New(ctx context.Context, h host.Host, e *engine.Engine) *libp2pserver.Server {
	return libp2pserver.New(ctx, h, newHandler(e))
}
