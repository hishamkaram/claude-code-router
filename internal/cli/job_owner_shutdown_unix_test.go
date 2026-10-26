//go:build linux || darwin

package cli

import (
	"errors"
	"net/http"
	"testing"
)

func TestCloseJobControlServerPreservesUnexpectedServeErrors(t *testing.T) {
	t.Parallel()
	unexpected := errors.New("dummy listener failure")
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "normal close", err: http.ErrServerClosed},
		{name: "unexpected failure", err: unexpected},
	} {
		t.Run(test.name, func(t *testing.T) {
			served := make(chan error, 1)
			served <- test.err
			err := closeJobControlServer(&http.Server{}, served)
			if errors.Is(test.err, http.ErrServerClosed) {
				if err != nil {
					t.Fatalf("normal server close reported an error: %v", err)
				}
			} else if !errors.Is(err, unexpected) {
				t.Fatalf("server close lost unexpected error: %v", err)
			}
		})
	}
}
