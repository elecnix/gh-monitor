package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/elecnix/gh-monitor/backend"
)

// ServerConfig describes the backend being served. Leave Reader nil to serve
// notifications only — gh-monitor then keeps reads with its built-in backend,
// which is usually what a backend that is told about changes wants.
type ServerConfig struct {
	// Name identifies this backend to gh-monitor.
	Name string
	// Kinds are the target kinds served. Empty means every kind, which is a
	// strong claim: gh-monitor will route everything here.
	Kinds []backend.Kind
	// Source serves watch requests. Optional.
	Source backend.Source
	// Reader serves read requests. Optional.
	Reader backend.Reader
	// The mutation capabilities. Each is optional and independent: leave one
	// nil and gh-monitor keeps that verb with its built-in backend.
	Threads   backend.ThreadActor
	Review    backend.ReviewActor
	Comments  backend.CommentActor
	Draft     backend.DraftActor
	Reactions backend.ReactionActor
}

func (c ServerConfig) capabilities() []backend.Capability {
	var caps []backend.Capability
	if c.Source != nil {
		caps = append(caps, backend.CapSource)
	}
	if c.Reader != nil {
		caps = append(caps, backend.CapReader)
	}
	if c.Threads != nil {
		caps = append(caps, backend.CapThreads)
	}
	if c.Review != nil {
		caps = append(caps, backend.CapReview)
	}
	if c.Comments != nil {
		caps = append(caps, backend.CapComments)
	}
	if c.Draft != nil {
		caps = append(caps, backend.CapDraft)
	}
	if c.Reactions != nil {
		caps = append(caps, backend.CapReactions)
	}
	return caps
}

// Serve handles one client connection: it announces the backend, reads the
// single request, and streams the answer. It returns when the request is
// finished, the connection drops, or ctx is cancelled.
//
// This is the whole server side. A Go backend implements backend.Source
// and/or backend.Reader, accepts connections, and hands each one to Serve.
func Serve(ctx context.Context, conn io.ReadWriter, cfg ServerConfig) error {
	caps := cfg.capabilities()
	if len(caps) == 0 {
		return fmt.Errorf("serve %q: no capabilities configured", cfg.Name)
	}
	if cfg.Name == "" {
		return fmt.Errorf("serve: the backend must name itself")
	}

	if err := writeJSON(conn, Hello{
		Protocol:     Protocol,
		Name:         cfg.Name,
		Capabilities: caps,
		Kinds:        cfg.Kinds,
	}); err != nil {
		return fmt.Errorf("write hello: %w", err)
	}

	br := bufio.NewReader(conn)
	var req Request
	if err := readJSON(br, &req); err != nil {
		return fmt.Errorf("read request: %w", err)
	}

	// The client sends nothing after its request, so anything further on the
	// connection means it has gone away. Watching for that is what releases a
	// long watch when the client stops caring — without it, a shared poller
	// would keep fetching for a process that has already exited.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, br)
		cancel()
	}()

	switch req.Op {
	case OpWatch:
		return serveWatch(ctx, conn, cfg, req)
	case OpRead:
		return serveRead(ctx, conn, cfg, req)
	}
	if handled, err := serveMutation(ctx, conn, cfg, req); handled {
		return err
	}
	return writeJSON(conn, Frame{Error: fmt.Sprintf("unsupported op %q", req.Op)})
}

func serveWatch(ctx context.Context, conn io.Writer, cfg ServerConfig, req Request) error {
	if cfg.Source == nil {
		return writeJSON(conn, Frame{Error: fmt.Sprintf("backend %q does not provide a source", cfg.Name)})
	}
	ch, err := cfg.Source.Watch(ctx, req.Target, req.Options)
	if err != nil {
		return writeJSON(conn, Frame{Error: err.Error()})
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case u, open := <-ch:
			if !open {
				// The end-of-stream frame is what tells the client the watch
				// finished rather than broke.
				return writeJSON(conn, Frame{Done: true})
			}
			update := u
			if err := writeJSON(conn, Frame{Update: &update}); err != nil {
				return err
			}
		}
	}
}

func serveRead(ctx context.Context, conn io.Writer, cfg ServerConfig, req Request) error {
	if cfg.Reader == nil {
		return writeJSON(conn, Frame{Error: fmt.Sprintf("backend %q does not provide a reader", cfg.Name)})
	}
	st, err := cfg.Reader.Read(ctx, req.Target)
	if err != nil {
		return writeJSON(conn, Frame{Error: err.Error()})
	}
	var raw json.RawMessage
	if st != nil {
		b, err := json.Marshal(st)
		if err != nil {
			return writeJSON(conn, Frame{Error: fmt.Sprintf("encode status: %v", err)})
		}
		raw = b
	}
	return writeJSON(conn, Frame{Status: raw})
}
