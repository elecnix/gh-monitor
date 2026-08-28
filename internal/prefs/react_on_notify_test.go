package prefs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reactOnNotify is the eyes-on-notify switch: on by default, so a delivered
// notification's comments get a 👀 reaction unless the operator opts out.
func TestReactOnNotifyDefaultTrue(t *testing.T) {
	p := DefaultPreferences()
	assert.True(t, p.ReactOnNotify)
}

func TestLoadOmitsReactOnNotifyKeepsDefault(t *testing.T) {
	base := t.TempDir()
	p, err := Load(base)
	require.NoError(t, err)
	assert.True(t, p.ReactOnNotify, "absence of the key keeps the default (true)")
}

func TestUpdateFileSetsReactOnNotify(t *testing.T) {
	base := t.TempDir()
	eff, err := UpdateFile(base, []byte(`{"reactOnNotify":false}`))
	require.NoError(t, err)
	assert.False(t, eff.ReactOnNotify)
	// null resets to the default (true) and removes the key from the file.
	eff, err = UpdateFile(base, []byte(`{"reactOnNotify":null}`))
	require.NoError(t, err)
	assert.True(t, eff.ReactOnNotify)
	stored := readStoredFile(t, base)
	_, present := stored["reactOnNotify"]
	assert.False(t, present, "null reactOnNotify should be removed from file")
}
