//go:build !linux && !darwin

package jobs

import "os"

func openOutputFile(string) (*os.File, error) {
	return nil, &OutputError{ReasonCode: ReasonObservationFailed}
}
