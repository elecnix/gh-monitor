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

	var hello Hello
	if err := readJSON(bufio.NewReader(conn), &hello); err != nil {
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
func (p *Provider) Watch(ctx context.Context, t backend.Target, opts backend.WatchOptions) (<-chan backend.Update, error) {
	conn, err := p.transport.Open(ctx)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)

	var hello Hello
	if err := readJSON(br, &hello); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read hello from %s: %w", p.transport, err)
	}
	if err := validateHello(hello); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%s: %w", p.transport, err)
	}
	if err := writeJSON(conn, Request{Op: OpWatch, Target: t, Options: opts}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send watch request to %s: %w", p.transport, err)
	}

	out := make(chan backend.Update, 16)
	go func() {
		defer close(out)
		defer func() { _ = conn.Close() }()

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

		// Closing the connection is what unblocks the read below when the
		// caller cancels, so a watch on a quiet target still stops promptly.
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-done:
			}
		}()

		for {
			var frame Frame
			if err := readJSON(br, &frame); err != nil {
				if ctx.Err() != nil {
					return // the caller stopped watching; nothing to report
				}
				if errors.Is(err, io.EOF) {
					err = errors.New("stream ended without an end-of-stream frame")
				}
				degrade(err)
				return
			}
			switch {
			case frame.Error != "":
				degrade(errors.New(frame.Error))
				return
			case frame.Done:
				return
			case frame.Update != nil:
				if !emit(*frame.Update) {
					return
				}
				if frame.Update.Terminal {
					return
				}
			}
		}
	}()
	return out, nil
}

// Read opens a connection and asks the server for the target's current status.
func (p *Provider) Read(ctx context.Context, t backend.Target) (backend.Status, error) {
	conn, err := p.transport.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)

	var hello Hello
	if err := readJSON(br, &hello); err != nil {
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
