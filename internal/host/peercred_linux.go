//go:build linux

package host

import (
	"net"

	"golang.org/x/sys/unix"
)

func peerCredentials(c *net.UnixConn) (peerCred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return peerCred{}, err
	}
	var cred *unix.Ucred
	var credErr error
	if err = raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return peerCred{}, err
	}
	if credErr != nil {
		return peerCred{}, credErr
	}
	return peerCred{uid: cred.Uid, pid: int(cred.Pid)}, nil
}
