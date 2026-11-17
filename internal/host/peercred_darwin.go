//go:build darwin

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
	var cred *unix.Xucred
	var credErr error
	if err = raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return peerCred{}, err
	}
	if credErr != nil {
		return peerCred{}, credErr
	}
	return peerCred{uid: cred.Uid}, nil
}
