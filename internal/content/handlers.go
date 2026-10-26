package content

import (
	"context"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/server"
	"github.com/mhk-os/server/internal/wire"
)

// Register adds the ContentHello and ContentManifests handlers and forgets announcements of closed connections.
func (c *Catalog) Register(srv *server.Server) {
	srv.Handle(proto.TypeContentHello, true, c.handleHello)
	srv.Handle(proto.TypeContentManifests, true, c.handleManifests)
	srv.OnClose(func(conn *server.Conn) { c.Forget(conn) })
}

func (c *Catalog) handleHello(ctx context.Context, conn *server.Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.ContentHello
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	// Lobbies match players by the simulation version of their Hello; a different one here is a client bug.
	if req.SimVersion != conn.Hello().SimVersion {
		return nil, server.Errorf(proto.StatusBadRequest, "simVersion differs from the one in Hello.")
	}
	id, ok := conn.AccountID()
	if !ok {
		return nil, server.Errorf(proto.StatusNotLoggedIn, "Please log in first.")
	}
	return c.Hello(ctx, conn, id, req.SimVersion, req.Packs)
}

func (c *Catalog) handleManifests(ctx context.Context, conn *server.Conn, r *wire.Reader) (proto.Message, error) {
	var req proto.ContentManifests
	if err := req.Decode(r); err != nil {
		return nil, err
	}
	id, ok := conn.AccountID()
	if !ok {
		return nil, server.Errorf(proto.StatusNotLoggedIn, "Please log in first.")
	}
	return nil, c.Manifests(ctx, conn, id, req.Manifests)
}
