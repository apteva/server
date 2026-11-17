//go:build !linux && !darwin

package host

import "net"

// Without kernel peer credentials every control request is refused.
func peerCredentials(*net.UnixConn) (peerCred, error) { return peerCred{}, errNoPeerCred }
