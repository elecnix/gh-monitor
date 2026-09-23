package backend

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBatchMarksEveryUpdateButTheLast(t *testing.T) {
	var got []Update
	add, flush := Batch(func(u Update) { got = append(got, u) })
	add(Update{Event: Event{Type: EventNewFailingChecks}})
	add(Update{Event: Event{Type: EventNewUnresolvedThreads}})
	add(Update{Event: Event{Type: EventCheckAnnotations}})
	require.Len(t, got, 2, "the last update waits for flush")
	flush()

	require.Len(t, got, 3)
	require.True(t, got[0].More)
	require.True(t, got[1].More)
	require.False(t, got[2].More)
	require.Equal(t, EventCheckAnnotations, got[2].Event.Type)
}

func TestBatchFlushStartsTheNextBatch(t *testing.T) {
	var got []Update
	add, flush := Batch(func(u Update) { got = append(got, u) })
	flush() // an observation with no updates emits nothing
	require.Empty(t, got)

	add(Update{Event: Event{Type: EventMerged}})
	flush()
	add(Update{Event: Event{Type: EventConflict}})
	flush()
	require.Len(t, got, 2)
	require.False(t, got[0].More, "a one-update batch carries no More")
	require.False(t, got[1].More)
}
