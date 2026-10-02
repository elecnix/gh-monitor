package report

import "time"

// FilterOptions controls shaping of reviews and threads. It is the shaping
// half of Options; the rest of that struct decides what is asked for at all.
type FilterOptions struct {
	Reviewer             string
	States               []State
	RequireUnresolved    bool
	RequireNotOutdated   bool
	TailReplies          int
	IncludeCommentNodeID bool
	Author               string
	IncludeResolved      bool
}

// Review models a pull request review fetched from GraphQL.
type Review struct {
	ID          string
	State       State
	Body        *string
	SubmittedAt *time.Time
	AuthorLogin string
	DatabaseID  *int
}

// Thread captures a review thread and its constituent comments.
type Thread struct {
	ID         string
	Path       string
	Line       *int
	IsResolved bool
	IsOutdated bool
	Comments   []ThreadComment
}

// ThreadComment represents a single comment node within a thread.
type ThreadComment struct {
	NodeID             string
	DatabaseID         int
	Body               string
	CreatedAt          time.Time
	AuthorLogin        string
	ReviewDatabaseID   *int
	ReplyToDatabaseID  *int
	ReplyToCommentNode *string
}
