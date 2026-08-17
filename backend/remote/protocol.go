// Package remote connects gh-monitor to a backend running as a separate
// process or service.
//
// The protocol is line-delimited JSON, chosen so a backend can be written in
// any language: on connect the server announces what it provides, the client
// sends one request, and the server streams the answer.
//
//	server → {"protocol":1,"name":"relay","capabilities":["source"],"kinds":["pr","run"]}
//	client → {"op":"watch","target":{...},"options":{...}}
//	server → {"update":{...}}
//	server → {"update":{...}}
//	server → {"done":true}
//
// The hello is how a backend registers only part of the surface: it names the
// capabilities it implements and the target kinds it covers, and gh-monitor
// leaves everything else to the backends that do cover it.
//
// A Go backend can serve this protocol with Serve; see docs/BACKENDS.md.
//
// This package imports nothing but the backend API, so an out-of-tree module
// can use both.
package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/elecnix/gh-monitor/backend"
)

// Protocol is the version this package speaks. A server announcing a
// different major version is refused rather than guessed at.
const Protocol = 1

// HandshakeTimeout bounds the wait for a server's opening frame.
//
// It exists because a peer that never says hello would otherwise block
// forever, and there is one such peer in the wild: a gh-monitor daemon from a
// build before this protocol, which holds the socket and waits for the client
// to speak first. Without a bound, meeting one hangs the client with no output
// and no error — the worst thing a monitor can do.
const HandshakeTimeout = 2 * time.Second

// readHello reads the server's opening frame, giving up after
// HandshakeTimeout. On timeout it closes the connection, both to release the
// peer and to unblock the goroutine still parked in the read.
func readHello(ctx context.Context, conn io.Closer, br *bufio.Reader) (Hello, error) {
	type result struct {
		hello Hello
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		var h Hello
		err := readJSON(br, &h)
		ch <- result{h, err}
	}()

	select {
	case r := <-ch:
		return r.hello, r.err
	case <-time.After(HandshakeTimeout):
		_ = conn.Close()
		return Hello{}, fmt.Errorf("no protocol hello within %s (is this a gh-monitor backend?)", HandshakeTimeout)
	case <-ctx.Done():
		_ = conn.Close()
		return Hello{}, ctx.Err()
	}
}

// Operations a client can request.
const (
	// OpWatch asks for a stream of updates until the target is terminal.
	OpWatch = "watch"
	// OpRead asks for the target's current status, once.
	OpRead = "read"
)

// Hello is the server's opening frame, declaring what it provides.
type Hello struct {
	Protocol int    `json:"protocol"`
	Name     string `json:"name"`
	// Capabilities lists what the server implements. A server that omits
	// CapReader keeps reads with the built-in backend.
	Capabilities []backend.Capability `json:"capabilities"`
	// Kinds lists the target kinds the server covers. Empty means every kind.
	Kinds []backend.Kind `json:"kinds,omitempty"`
}

// Request is the client's single frame following the hello.
type Request struct {
	Op      string               `json:"op"`
	Target  backend.Target       `json:"target"`
	Options backend.WatchOptions `json:"options,omitempty"`
	// Payload carries a mutation's arguments, encoded as JSON.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Frame is one server response line. Exactly one field is meaningful per
// frame.
type Frame struct {
	// Update carries one change (OpWatch).
	Update *backend.Update `json:"update,omitempty"`
	// Status carries the encoded status (OpRead).
	Status json.RawMessage `json:"status,omitempty"`
	// Error ends the stream with a failure the client reports as degraded.
	Error string `json:"error,omitempty"`
	// Done ends the stream cleanly.
	Done bool `json:"done,omitempty"`
	// Result carries a mutation's return value, encoded as JSON.
	Result json.RawMessage `json:"result,omitempty"`
}

// writeJSON writes one newline-delimited JSON frame.
func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// jsonUnmarshal is a thin alias so the package's own decoding helper is the
// single place JSON handling can be adjusted.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// readJSON reads one newline-delimited JSON frame.
func readJSON(r io.Reader, v any) error {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return err
	}
	return jsonUnmarshal(line, v)
}

// validateHello checks a server's opening frame before anything is registered
// on the strength of it.
func validateHello(h Hello) error {
	if h.Protocol != Protocol {
		return fmt.Errorf("backend speaks protocol %d, this build speaks %d", h.Protocol, Protocol)
	}
	if h.Name == "" {
		return fmt.Errorf("backend did not name itself")
	}
	if len(h.Capabilities) == 0 {
		return fmt.Errorf("backend %q declares no capabilities", h.Name)
	}
	for _, c := range h.Capabilities {
		if !knownCapabilities[c] {
			return fmt.Errorf("backend %q declares unknown capability %q", h.Name, c)
		}
	}
	for _, k := range h.Kinds {
		if _, err := backend.ParseKind(string(k)); err != nil {
			return fmt.Errorf("backend %q: %w", h.Name, err)
		}
	}
	return nil
}

// has reports whether the hello declares a capability.
func (h Hello) has(c backend.Capability) bool {
	for _, got := range h.Capabilities {
		if got == c {
			return true
		}
	}
	return false
}

// knownCapabilities is what a server may declare. Anything else is rejected:
// a capability this build does not understand would be registered and then
// never resolved, which reads as a backend that is present but silent.
var knownCapabilities = map[backend.Capability]bool{
	backend.CapSource:    true,
	backend.CapReader:    true,
	backend.CapThreads:   true,
	backend.CapReview:    true,
	backend.CapComments:  true,
	backend.CapDraft:     true,
	backend.CapReactions: true,
}
