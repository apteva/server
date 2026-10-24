//go:build !linux && !darwin

package host

import "errors"

func Lock(string, bool) (func(), error) {
	return nil, errors.New("host deployments require Linux or macOS")
}
