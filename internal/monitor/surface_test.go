package monitor

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSurfaceVocabulary pins the identifiers that reach notifications and the
// shed_surfaces JSON field. Renaming one is a breaking change for every caller
// parsing them, so a change here must be a deliberate edit of this test.
func TestSurfaceVocabulary(t *testing.T) {
	assert.Equal(t, "annotations", string(SurfaceAnnotations))
	assert.Equal(t, "reviews", string(SurfaceReviews))
	assert.Equal(t, "review threads", string(SurfaceReviewThreads))
	assert.Equal(t, "comments", string(SurfaceComments))
}

func TestSurfaces_Has(t *testing.T) {
	all := allSurfaces()
	for _, s := range all {
		assert.True(t, all.Has(s), "%q is in the list", s)
	}
	assert.False(t, Surfaces{SurfaceReviews}.Has(SurfaceComments))
	assert.False(t, Surfaces(nil).Has(SurfaceReviews))
}

func TestSurfaces_NamesAndString(t *testing.T) {
	s := Surfaces{SurfaceAnnotations, SurfaceReviews}
	assert.Equal(t, []string{"annotations", "reviews"}, s.Names(), "Names projects the []string shape")
	assert.Equal(t, "annotations, reviews", s.String(), "String is the notice rendering")

	// A caller that assigns to a []string field gets a real conversion, and
	// an unknown value in a stored baseline still round-trips.
	names := s.Names()
	assert.Equal(t, "reviews", names[1])

	var empty Surfaces
	assert.Nil(t, empty.Names(), "a nil list projects to nil so omitempty still applies")
	assert.Empty(t, empty.String(), "an empty list renders empty, never a dangling separator")
}

// TestShedSurfacesJSONUnchanged is the compatibility guard for the typed
// vocabulary: shed_surfaces on the wire is still an array of plain strings.
func TestShedSurfacesJSONUnchanged(t *testing.T) {
	blob, err := json.Marshal(PRStatus{ShedSurfaces: TierStatus.ShedSurfaces()})
	require.NoError(t, err)
	assert.Contains(t, string(blob), `"shed_surfaces":["annotations","reviews","review threads","comments"]`)

	// A baseline written by an older daemon parses back into the vocabulary.
	var back PRStatus
	require.NoError(t, json.Unmarshal([]byte(`{"shed_surfaces":["annotations","reviews"]}`), &back))
	assert.Equal(t, Surfaces{SurfaceAnnotations, SurfaceReviews}, back.ShedSurfaces)

	// Nothing shed stays omitted.
	blob, err = json.Marshal(PRStatus{ShedSurfaces: TierFull.ShedSurfaces()})
	require.NoError(t, err)
	assert.NotContains(t, string(blob), "shed_surfaces")
}

// allSurfaces is every surface the vocabulary can name, in the operator's
// priority order (least valuable first). Tests that must reason about the
// whole vocabulary iterate it, so a new constant shows up in their coverage.
func allSurfaces() Surfaces {
	return Surfaces{SurfaceAnnotations, SurfaceReviews, SurfaceReviewThreads, SurfaceComments}
}

// TestShedVocabularyCoversEverySurface is the test the string literals could
// not have. It walks every tier's shed list and checks that CarryForwardShed
// actually carries the snapshot fields behind each named surface — and leaves
// the ones the tier still fetches alone. Before the typed vocabulary a
// misspelled literal produced a silent no-op here: the surface was named in
// the tier but nothing was carried forward, so a shed APPROVED review read as
// dismissed. A surface named in the list but unhandled here, or handled here
// but never named, is now a test failure rather than a runtime surprise.
func TestShedVocabularyCoversEverySurface(t *testing.T) {
	prev := &PRStatus{
		State:                "OPEN",
		ReviewDecision:       "APPROVED",
		ReviewAuthor:         "alice",
		UnresolvedThreads:    []ThreadSummary{{ID: "t1"}},
		GeneralComments:      []GeneralComment{{ID: "c1"}},
		CheckAnnotations:     []AnnotationSummary{{CheckName: "ci", Title: "x"}},
		AnnotationsTruncated: true,
		AnnotationsURL:       "https://example.invalid/run/1",
	}

	tiers := []QueryTier{TierStatus, TierNoReviews, TierNoAnnotations}
	for _, tier := range tiers {
		t.Run(tier.String(), func(t *testing.T) {
			shed := tier.ShedSurfaces()
			require.NotEmpty(t, shed, "%s sheds something", tier)

			// The snapshot fields each surface owns, and whether
			// CarryForwardShed restored them from prev.
			restored := map[Surface]func(c *PRStatus) bool{
				SurfaceAnnotations: func(c *PRStatus) bool {
					return assert.ObjectsAreEqual(prev.CheckAnnotations, c.CheckAnnotations) &&
						c.AnnotationsTruncated == prev.AnnotationsTruncated &&
						c.AnnotationsURL == prev.AnnotationsURL
				},
				SurfaceReviews: func(c *PRStatus) bool {
					return c.ReviewDecision == prev.ReviewDecision && c.ReviewAuthor == prev.ReviewAuthor
				},
				SurfaceReviewThreads: func(c *PRStatus) bool {
					return assert.ObjectsAreEqual(prev.UnresolvedThreads, c.UnresolvedThreads)
				},
				SurfaceComments: func(c *PRStatus) bool {
					return assert.ObjectsAreEqual(prev.GeneralComments, c.GeneralComments)
				},
			}

			c := &PRStatus{State: "OPEN", ShedSurfaces: shed}
			CarryForwardShed(prev, c)

			for _, surface := range shed {
				probe, ok := restored[surface]
				require.True(t, ok, "the tier names shed surface %q, which CarryForwardShed does not handle", surface)
				assert.True(t, probe(c), "%q is shed at %s, so its values must be carried forward", surface, tier)
			}
			for _, surface := range allSurfaces() {
				if shed.Has(surface) {
					continue
				}
				// Every surface needs a probe, or this loop calls a nil func
				// and takes the binary down instead of reporting the surface
				// it could not check. The shed loop above cannot catch that:
				// it only visits surfaces this tier sheds.
				probe, ok := restored[surface]
				require.True(t, ok, "no probe for surface %q, so the test cannot check it", surface)
				// A surface the tier still fetches keeps whatever the fetch
				// produced; an empty snapshot must not be back-filled.
				assert.False(t, probe(c), "%q is still fetched at %s, so it must not be carried", surface, tier)
			}
		})
	}
}
