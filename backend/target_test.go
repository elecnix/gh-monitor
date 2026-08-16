package backend

import "testing"

func TestTargetString(t *testing.T) {
	tests := []struct {
		name   string
		target Target
		want   string
	}{
		{
			name:   "pull request",
			target: Target{Kind: KindPR, Owner: "elecnix", Repo: "gh-monitor", Number: 42},
			want:   "elecnix/gh-monitor#42",
		},
		{
			name:   "issue",
			target: Target{Kind: KindIssue, Owner: "elecnix", Repo: "gh-monitor", Number: 7},
			want:   "elecnix/gh-monitor#7",
		},
		{
			name:   "ref",
			target: Target{Kind: KindRef, Owner: "elecnix", Repo: "gh-monitor", Ref: "main"},
			want:   "elecnix/gh-monitor@main",
		},
		{
			name:   "commit is abbreviated to seven characters",
			target: Target{Kind: KindCommit, Owner: "elecnix", Repo: "gh-monitor", SHA: "0123456789abcdef"},
			want:   "elecnix/gh-monitor@0123456",
		},
		{
			name:   "short commit is not truncated",
			target: Target{Kind: KindCommit, Owner: "elecnix", Repo: "gh-monitor", SHA: "abc"},
			want:   "elecnix/gh-monitor@abc",
		},
		{
			name:   "workflow run",
			target: Target{Kind: KindRun, Owner: "elecnix", Repo: "gh-monitor", RunID: 99},
			want:   "elecnix/gh-monitor run #99",
		},
		{
			name:   "repository",
			target: Target{Kind: KindRepo, Owner: "elecnix", Repo: "gh-monitor"},
			want:   "elecnix/gh-monitor",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.target.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTargetHostname(t *testing.T) {
	if got := (Target{}).Hostname(); got != "github.com" {
		t.Fatalf("empty Host should default to github.com, got %q", got)
	}
	if got := (Target{Host: "ghe.example.com"}).Hostname(); got != "ghe.example.com" {
		t.Fatalf("Hostname() = %q, want ghe.example.com", got)
	}
}

func TestParseKind(t *testing.T) {
	for _, k := range AllKinds() {
		got, err := ParseKind(string(k))
		if err != nil {
			t.Fatalf("ParseKind(%q) returned error: %v", k, err)
		}
		if got != k {
			t.Fatalf("ParseKind(%q) = %q", k, got)
		}
	}
	if _, err := ParseKind("nope"); err == nil {
		t.Fatal("ParseKind should reject an unknown kind")
	}
	// Kinds are normalised so callers can pass either case.
	if got, err := ParseKind("  PR "); err != nil || got != KindPR {
		t.Fatalf("ParseKind(\"  PR \") = %q, %v", got, err)
	}
}
