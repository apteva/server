//go:build !linux && !darwin

package admission

import "errors"

func takeLease(dir, key string, limit int) (func(), error) {
	if dir == "" {
		return func() {}, nil
	}
	return nil, errors.New("shared automatic admission leases require Linux or Darwin")
}
