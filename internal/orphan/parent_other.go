//go:build !linux

package orphan

// procParentOf is unavailable on this platform, so ResolveLauncher reports no
// resolution and the guard falls back to the immediate parent — the behaviour
// it had before wrappers were considered.
func procParentOf(int) (int, string, bool) { return 0, "", false }
