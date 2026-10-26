//go:build !windows

package sdnotify

import "net"

// post sends one datagram to the manager socket. A datagram carries the whole
// message, so the manager reads one assignment per write and needs no framing.
func post(name string, payload []byte) error {
	conn, err := net.Dial("unixgram", name)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write(payload)
	return err
}
