//go:build !darwin && !linux

package host

import "errors"

func preflightSpace(string, string) error {
	return errors.New("safe disk capacity checks are unavailable on this platform")
}
