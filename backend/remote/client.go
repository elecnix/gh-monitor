package remote

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/elecnix/gh-monitor/backend"
)

// Provider is a backend reached over a Transport. It registers exactly the
// capabilities and kinds the server declared in its hello — no more, so the
// built-in backend keeps whatever the server left alone.
type Provider struct {
	transport Transport
	hello     Hello
}

// Connect opens a probe connection, reads the server's hello, and returns a
// Provider describing what that server covers. The probe is closed
// immediately; each Watch and Read opens its own connection.
func Connect(ctx context.Context, tr Transport) (*Provider, error) {
	conn, err := tr.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	hello, err := readHello(ctx, conn, bufio.NewReader(conn))
	if err != nil {
		return nil, fmt.Errorf("read hello from %s: %w", tr, err)
	}
	if err := validateHello(hello); err != nil {
		return nil, fmt.Errorf("%s: %w", tr, err)
	}
	return &Provider{transport: tr, hello: hello}, nil
}

// Name is the name the server gave itself.
func (p *Provider) Name() string { return p.hello.Name }

// Kinds are the target kinds the server covers; nil means every kind.
func (p *Provider) Kinds() []backend.Kind { return p.hello.Kinds }

// Register adds only the capabilities the server declared.
func (p *Provider) Register(r *backend.Registry) error {
	if p.hello.has(backend.CapSource) {
		r.RegisterSource(p.hello.Name, p.hello.Kinds, backend.SourceFunc(p.Watch))
	}
	if p.hello.has(backend.CapReader) {
		r.RegisterReader(p.hello.Name, p.hello.Kinds, backend.ReaderFunc(p.Read))
	}
	if p.hello.has(backend.CapThreads) {
		r.RegisterThreads(p.hello.Name, p.hello.Kinds, p)
	}
	if p.hello.has(backend.CapReview) {
		r.RegisterReview(p.hello.Name, p.hello.Kinds, p)
	}
	if p.hello.has(backend.CapComments) {
		r.RegisterComments(p.hello.Name, p.hello.Kinds, p)
	}
	if p.hello.has(backend.CapDraft) {
		r.RegisterDraft(p.hello.Name, p.hello.Kinds, p)
	}
	if p.hello.has(backend.CapReactions) {
		r.RegisterReactions(p.hello.Name, p.hello.Kinds, p)
	}
	return nil
}

// Watch opens a connection, requests a stream for t, and delivers each update
// the server sends.
//
// The stream ending for any reason other than a clean end-of-stream is
// reported as an EventDegraded update. A watcher that simply went quiet would
// be indistinguishable from a target where nothing is happening, and that is
// the one thing a monitor must never be.
//
// When the server announced itself Resumable, a broken stream is not the end:
// the watcher re-establishes the stream with the same ResumeID with bounded
// backoff for as long as the watch is alive, and the server resumes from the
// baseline it held for that watcher instead of replaying what it already
// delivered. This is what lets a resident watcher ride across a daemon
// upgrade handoff (issue #73) instead of dying of it.
func (p *Provider) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	conn, br, err := p.openWatch(ctx, t, opts)
	if err != nil {
		return nil, err
	}

	out := make(chan backend.Update, 16)
	go func() {
		defer close(out)
		p.pumpWatch(ctx, t, opts, conn, br, out)
	}()
	return out, nil
}

// openWatch dials the server, reads its hello, and sends the watch request.
func (p *Provider) openWatch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (io.ReadWriteCloser, *bufio.Reader, error) {
	conn, err := p.transport.Open(ctx)
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReader(conn)

	hello, err := readHello(ctx, conn, br)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("read hello from %s: %w", p.transport, err)
	}
	if err := validateHello(hello); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("%s: %w", p.transport, err)
	}
	if err := writeJSON(conn, Request{Op: OpWatch, Target: t, Options: opts}); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("send watch request to %s: %w", p.transport, err)
	}
	return conn, br, nil
}

// reconnectBackoff starts and caps the delay between attempts to re-establish
// a dropped stream. The floor is short enough that an upgrade handoff — where
// the successor daemon is already serving — resumes within a beat; the cap
// keeps a daemon that is gone for good from being polled into oblivion.
const (
	reconnectStart = 250 * time.Millisecond
	reconnectCap   = 5 * time.Second
)

// pumpWatch streams frames to out until the watch ends cleanly, the caller
// cancels, or — when the server is not resumable — the stream breaks.
func (p *Provider) pumpWatch(ctx context.Context, t backend.Target, opts backend.WatchOptions, conn io.ReadWriteCloser, br *bufio.Reader, out chan<- backend.Update) {
	emit := func(u backend.Update) bool {
		select {
		case out <- u:
			return true
		case <-ctx.Done():
			return false
		}
	}
	degrade := func(err error) {
		emit(backend.Update{
			Target: t,
			Event: backend.Event{
				Type:            backend.EventDegraded,
				DegradedSurface: "backend",
				DegradedMessage: fmt.Sprintf("%s: %v", p.hello.Name, err),
			},
			At: time.Now(),
		})
	}

	backoff := reconnectStart
	for {
		// Closing the connection is what unblocks the read in stream when the
		// caller cancels, so a watch on a quiet target still stops promptly.
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-done:
			}
		}()

		clean, err := p.stream(ctx, t, br, emit)
		close(done)
		_ = conn.Close()
		if clean || ctx.Err() != nil {
			return
		}
		if !p.hello.Resumable {
			degrade(err)
			return
		}

		// Loud once per break — then quiet, bounded retrying. A watcher that
		// reappears without a word would look like silence; a watcher that
		// says nothing once it is back would look broken.
		degrade(fmt.Errorf("connection lost (%v); reconnecting", err))
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, reconnectCap)
			reconn, rbr, oerr := p.openWatch(ctx, t, opts)
			if oerr != nil {
				continue
			}
			emit(backend.Update{
				Target: t,
				Event: backend.Event{
					Type:   backend.EventDegraded,
					Notice: fmt.Sprintf("✅ reconnected to %s; resuming the watch from where it left off", p.hello.Name),
				},
				At: time.Now(),
			})
			conn, br = reconn, rbr
			backoff = reconnectStart
			break
		}
	}
}

// stream reads frames from an established watch until it ends cleanly
// (end-of-stream, terminal update, server error frame, or caller cancel —
// clean=true) or the transport breaks (clean=false, with the error).
func (p *Provider) stream(ctx context.Context, t backend.Target, br *bufio.Reader, emit func(backend.Update) bool) (bool, error) {
	for {
		var frame Frame
		if err := readJSON(br, &frame); err != nil {
			if ctx.Err() != nil {
				return true, nil // the caller stopped watching; nothing to report
			}
			if errors.Is(err, io.EOF) {
				err = errors.New("stream ended without an end-of-stream frame")
			}
			return false, err
		}
		switch {
		case frame.Error != "":
			degrade := errors.New(frame.Error)
			emit(backend.Update{
				Target: t,
				Event: backend.Event{
					Type:            backend.EventDegraded,
					DegradedSurface: "backend",
					DegradedMessage: fmt.Sprintf("%s: %v", p.hello.Name, degrade),
				},
				At: time.Now(),
			})
			return true, nil
		case frame.Done:
			return true, nil
		case frame.Update != nil:
			if !emit(*frame.Update) {
				return true, nil
			}
			if frame.Update.Terminal {
				return true, nil
			}
		}
	}
}

// Read opens a connection and asks the server for the target's current status.
func (p *Provider) Read(ctx context.Context, t backend.Target) (backend.Status, error) {
	conn, err := p.transport.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)

	if _, err := readHello(ctx, conn, br); err != nil {
		return nil, fmt.Errorf("read hello from %s: %w", p.transport, err)
	}
	if err := writeJSON(conn, Request{Op: OpRead, Target: t}); err != nil {
		return nil, fmt.Errorf("send read request to %s: %w", p.transport, err)
	}

	var frame Frame
	if err := readJSON(br, &frame); err != nil {
		return nil, fmt.Errorf("read response from %s: %w", p.transport, err)
	}
	if frame.Error != "" {
		return nil, fmt.Errorf("%s: %s", p.hello.Name, frame.Error)
	}
	return backend.DecodeStatus(t.Kind, frame.Status)
}
