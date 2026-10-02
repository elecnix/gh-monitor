package remote

import "github.com/elecnix/gh-monitor/backend"

// providerBinding binds one capability to both ends of the remote link: what a
// ServerConfig carries for it (which is what the hello announces), and what a
// connected Provider offers for it (which is what it registers).
//
// The table is keyed by the capability names in backend's descriptor, and
// TestCapabilityBindingsCoverEveryCapability fails the build when one is
// missing. Before it existed, a capability added to the descriptor and wired
// into ServerConfig but forgotten in Provider.Register validated cleanly,
// registered nothing, and read as a backend that was present and silent —
// which is precisely the outcome validateHello's own comment says it exists to
// prevent.
type providerBinding struct {
	// server reports the implementation a ServerConfig carries for the
	// capability, or nil when it carries none. Returning a nil actor interface
	// as an any yields a nil any, so the comparison against nil is the same
	// test the hand-written capabilities() branches used.
	server func(ServerConfig) any

	// client is the implementation a connected Provider offers. Source and
	// Reader are adapted to their interfaces because the Provider's Watch and
	// Read have the shape the backend package expects; the mutation
	// capabilities are implemented by *Provider itself.
	client func(*Provider) any

	// register adds the client's implementation to the registry under name,
	// for kinds.
	register func(r *backend.Registry, name string, kinds []backend.Kind, impl any)
}

// present returns v as an any that is nil when v is a nil interface value, so
// clientSelf is the Provider itself: *Provider implements every mutation
// capability, so they all offer the same value on the client side.
func clientSelf(p *Provider) any { return p }

// Returning a nil actor interface as an any yields a nil any, so the server:
// entries can hand the field straight back and the comparison against nil is
// the same test the hand-written capabilities() branches used.
var providerBindings = map[backend.Capability]providerBinding{
	backend.CapSource: {
		server: func(c ServerConfig) any { return c.Source },
		client: func(p *Provider) any { return backend.SourceFunc(p.Watch) },
		register: func(r *backend.Registry, n string, k []backend.Kind, i any) {
			r.RegisterSource(n, k, i.(backend.Source))
		},
	},
	backend.CapReader: {
		server: func(c ServerConfig) any { return c.Reader },
		client: func(p *Provider) any { return backend.ReaderFunc(p.Read) },
		register: func(r *backend.Registry, n string, k []backend.Kind, i any) {
			r.RegisterReader(n, k, i.(backend.Reader))
		},
	},
	backend.CapThreads: {
		server: func(c ServerConfig) any { return c.Threads },
		client: clientSelf,
		register: func(r *backend.Registry, n string, k []backend.Kind, i any) {
			r.RegisterThreads(n, k, i.(backend.ThreadActor))
		},
	},
	backend.CapReview: {
		server: func(c ServerConfig) any { return c.Review },
		client: clientSelf,
		register: func(r *backend.Registry, n string, k []backend.Kind, i any) {
			r.RegisterReview(n, k, i.(backend.ReviewActor))
		},
	},
	backend.CapComments: {
		server: func(c ServerConfig) any { return c.Comments },
		client: clientSelf,
		register: func(r *backend.Registry, n string, k []backend.Kind, i any) {
			r.RegisterComments(n, k, i.(backend.CommentActor))
		},
	},
	backend.CapDraft: {
		server: func(c ServerConfig) any { return c.Draft },
		client: clientSelf,
		register: func(r *backend.Registry, n string, k []backend.Kind, i any) {
			r.RegisterDraft(n, k, i.(backend.DraftActor))
		},
	},
	backend.CapReactions: {
		server: func(c ServerConfig) any { return c.Reactions },
		client: clientSelf,
		register: func(r *backend.Registry, n string, k []backend.Kind, i any) {
			r.RegisterReactions(n, k, i.(backend.ReactionActor))
		},
	},
}
