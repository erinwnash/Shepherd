//go:build linux

package control

import (
	"errors"
	"net"
	"os"
	"syscall"
)

// verifyPeerCred checks that the connecting process UID matches the controller's UID (ADR-0003).
func verifyPeerCred(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("not a unix socket connection")
	}
	rawConn, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var credErr error
	err = rawConn.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return err
	}
	if credErr != nil {
		return credErr
	}
	if int(cred.Uid) != os.Getuid() {
		return errors.New("peer uid does not match controller uid")
	}
	return nil
}
