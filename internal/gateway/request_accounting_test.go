package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestEntryPrecedesDecodingAndRouting(t *testing.T) {
	a := NewRequestAccounting()
	h := &handler{cfg: Config{Token: "local", RequestAccounting: a}}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("invalid JSON"))
	request.Header.Set("x-api-key", "local")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || a.Entered() != 1 {
		t.Fatalf("entry before decoding: status=%d entered=%d", response.Code, a.Entered())
	}
	if err := a.SealAndWait(t.Context()); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || a.Entered() != 1 {
		t.Fatalf("late request entered finalized job: %d/%d", response.Code, a.Entered())
	}
}

func TestRequestAccountingJoinsActiveRequestsAndCanRetryWait(t *testing.T) {
	a := NewRequestAccounting()
	for range 2 {
		if !a.begin() {
			t.Fatal("initial requests rejected")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.SealAndWait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unfinished requests were not retained: %v", err)
	}
	if a.begin() {
		t.Fatal("new request entered sealed accounting")
	}
	a.end()
	if err := a.SealAndWait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("one remaining request was lost: %v", err)
	}
	a.end()
	if err := a.SealAndWait(t.Context()); err != nil || a.Entered() != 2 {
		t.Fatalf("completed accounting: entered=%d err=%v", a.Entered(), err)
	}
}
