//go:build !darwin && !windows

package power

import "errors"

func acquire() (func(), error) {
	return nil, errors.New("sleep prevention is not supported on this platform")
}
