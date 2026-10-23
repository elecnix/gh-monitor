//go:build !unix

package activation

import "net"

// Listeners reports no passed listeners: socket activation is a Unix facility,
// so a daemon on this platform binds its own socket as it always has.
func Listeners(Env) ([]net.Listener, error) { return nil, nil }
