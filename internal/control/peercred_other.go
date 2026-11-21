//go:build !linux

package control

import (
	"net"
)

// verifyPeerCred is a fallback for non-Linux operating systems.
func verifyPeerCred(conn net.Conn) error {
	return nil
}
