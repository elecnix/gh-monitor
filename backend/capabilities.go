package backend

import "fmt"

// The remote wire operations, one per method on a mutation capability's
// interface. They live here rather than in the remote package because they are
// part of what a capability *is*: docs/BACKENDS.md publishes them beside the
// capability name, and the remote client and server dispatch on them. Naming
// them once means a capability cannot gain a method without also saying how it
// travels.
const (
	OpThreadsList      = "threads.list"
	OpThreadsView      = "threads.view"
	OpThreadsResolve   = "threads.resolve"
	OpThreadsUnresolve = "threads.unresolve"

	OpReviewStart         = "review.start"
	OpReviewAddComment    = "review.addComment"
	OpReviewUpdateComment = "review.updateComment"
	OpReviewDeleteComment = "review.deleteComment"
	OpReviewSubmit        = "review.submit"

	OpCommentsReply = "comments.reply"

	OpDraftStatus = "draft.status"
	OpDraftSet    = "draft.set"
	OpDraftList   = "draft.list"

	OpReactionsReact = "reactions.react"
)

// CapabilityInfo is the one declarative descriptor for a capability. Every
// place that has to know which capabilities exist reads it from here instead
// of keeping its own copy of the list: the Registry's canonical ordering, the
// remote hello's validation, the client's and server's wiring, and the tables
// in docs/BACKENDS.md.
//
// Adding a capability means adding a row to capabilityInfos. The remote wiring
// in backend/remote additionally needs a binding row, and a test there fails
// the build if one is missing — so a capability that a server announces but
// this build cannot wire is a compile-time error rather than a backend that
// registers successfully and then never resolves.
type CapabilityInfo struct {
	// Name is the capability's spelling: what a remote backend puts in its
	// hello, and what `gh monitor backends` prints.
	Name Capability

	// Interface is the fully qualified Go type a backend implements to provide
	// Name, without backticks. Summary and UsedBy are Markdown inline text
	// exactly as it should appear in the generated documentation.
	Interface string
	Summary   string
	UsedBy    string

	// Mutation reports whether Name is one of the mutation capabilities.
	// Mutation capabilities share one registration slice, resolve through the
	// same precedence, and travel over the wire as the Ops below. Source and
	// Reader are not mutations and have no ops.
	Mutation bool

	// Ops are the wire operations Name maps to, in canonical order. It is
	// empty for a capability that is not served over the wire as ops.
	Ops []string

	// register adds impl to the registry under Name. It stores the
	// implementation untyped, exactly as the mutation capabilities already do;
	// resolve rejects a mismatched type with the same error it has always
	// reported, so there is nothing to check here.
	register func(r *Registry, name string, kinds []Kind, impl any)

	// resolve returns the implementation of Name covering t.
	resolve func(r *Registry, t Target) (any, string, error)
}

// capabilityInfos is every capability in canonical order, and the only place
// one is declared. Look it up with LookupCapability; iterate it with
// AllCapabilities.
var capabilityInfos = []CapabilityInfo{
	{
		Name:      CapSource,
		Interface: "backend.Source",
		Summary:   "Delivers `Update`s describing what changed on a target",
		UsedBy:    "continuous watching",
		register:  func(r *Registry, name string, kinds []Kind, impl any) { r.RegisterSource(name, kinds, impl.(Source)) },
		resolve: func(r *Registry, t Target) (any, string, error) {
			return resolve(r.sources, t.Kind, r.pinned, CapSource)
		},
	},
	{
		Name:      CapReader,
		Interface: "backend.Reader",
		Summary:   "Returns a target's current `Status`",
		UsedBy:    "`--once`",
		register:  func(r *Registry, name string, kinds []Kind, impl any) { r.RegisterReader(name, kinds, impl.(Reader)) },
		resolve: func(r *Registry, t Target) (any, string, error) {
			return resolve(r.readers, t.Kind, r.pinned, CapReader)
		},
	},
	{
		Name:      CapThreads,
		Interface: "backend.ThreadActor",
		Summary:   "Lists, views, resolves review threads",
		UsedBy:    "`gh monitor threads`",
		Mutation:  true,
		Ops: []string{
			OpThreadsList, OpThreadsView, OpThreadsResolve, OpThreadsUnresolve,
		},
		register: func(r *Registry, name string, kinds []Kind, impl any) { r.registerActor(name, CapThreads, kinds, impl) },
		resolve: func(r *Registry, t Target) (any, string, error) {
			return resolveActor[ThreadActor](r, t.Kind, CapThreads)
		},
	},
	{
		Name:      CapReview,
		Interface: "backend.ReviewActor",
		Summary:   "Drives a pending review",
		UsedBy:    "`gh monitor review`",
		Mutation:  true,
		Ops: []string{
			OpReviewStart, OpReviewAddComment, OpReviewUpdateComment, OpReviewDeleteComment, OpReviewSubmit,
		},
		register: func(r *Registry, name string, kinds []Kind, impl any) { r.registerActor(name, CapReview, kinds, impl) },
		resolve: func(r *Registry, t Target) (any, string, error) {
			return resolveActor[ReviewActor](r, t.Kind, CapReview)
		},
	},
	{
		Name:      CapComments,
		Interface: "backend.CommentActor",
		Summary:   "Replies to review threads",
		UsedBy:    "`gh monitor comments`",
		Mutation:  true,
		Ops:       []string{OpCommentsReply},
		register: func(r *Registry, name string, kinds []Kind, impl any) {
			r.registerActor(name, CapComments, kinds, impl)
		},
		resolve: func(r *Registry, t Target) (any, string, error) {
			return resolveActor[CommentActor](r, t.Kind, CapComments)
		},
	},
	{
		Name:      CapDraft,
		Interface: "backend.DraftActor",
		Summary:   "Reads and changes draft status",
		UsedBy:    "`gh monitor draft`",
		Mutation:  true,
		Ops:       []string{OpDraftStatus, OpDraftSet, OpDraftList},
		register:  func(r *Registry, name string, kinds []Kind, impl any) { r.registerActor(name, CapDraft, kinds, impl) },
		resolve:   func(r *Registry, t Target) (any, string, error) { return resolveActor[DraftActor](r, t.Kind, CapDraft) },
	},
	{
		Name:      CapReactions,
		Interface: "backend.ReactionActor",
		Summary:   "Adds a reaction to a node",
		UsedBy:    "`gh monitor react`",
		Mutation:  true,
		Ops:       []string{OpReactionsReact},
		register: func(r *Registry, name string, kinds []Kind, impl any) {
			r.registerActor(name, CapReactions, kinds, impl)
		},
		resolve: func(r *Registry, t Target) (any, string, error) {
			return resolveActor[ReactionActor](r, t.Kind, CapReactions)
		},
	},
}

// LookupCapability returns the descriptor for c. The second result is false
// for a name this build does not know — which is how the remote protocol
// rejects a capability it could not honour even if it were registered.
func LookupCapability(c Capability) (CapabilityInfo, bool) {
	for _, info := range capabilityInfos {
		if info.Name == c {
			return info, true
		}
	}
	return CapabilityInfo{}, false
}

// AllCapabilities returns every capability in canonical order. The order is
// stable so `gh monitor backends`, the hello a remote server sends, and error
// messages all read the same way on every build.
func AllCapabilities() []Capability {
	out := make([]Capability, 0, len(capabilityInfos))
	for _, info := range capabilityInfos {
		out = append(out, info.Name)
	}
	return out
}

// IsMutation reports whether c is one of the mutation capabilities: the ones
// that share a registration slice and travel over the wire as named ops.
func IsMutation(c Capability) bool {
	info, ok := LookupCapability(c)
	return ok && info.Mutation
}

// RegisterCapability adds an implementation of c to the registry under name,
// for kinds (nil or empty meaning every kind). It is the untyped entry point
// the remote client uses; the typed RegisterX methods on Registry remain for
// in-tree backends that know their own type at the call site.
func (r *Registry) RegisterCapability(c Capability, name string, kinds []Kind, impl any) error {
	info, ok := LookupCapability(c)
	if !ok {
		return fmt.Errorf("unknown capability %q", c)
	}
	info.register(r, name, kinds, impl)
	return nil
}

// ResolveCapability returns the implementation of c covering t and the name of
// the backend it came from. It applies the same precedence as the typed XFor
// methods; those remain for callers that know the type they want.
func (r *Registry) ResolveCapability(c Capability, t Target) (any, string, error) {
	info, ok := LookupCapability(c)
	if !ok {
		return nil, "", fmt.Errorf("unknown capability %q", c)
	}
	return info.resolve(r, t)
}
