//go:build linux || darwin

package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Owner struct {
	mu     sync.Mutex
	store  Store
	record Record
	cancel context.CancelFunc
}

func NewOwner(store Store, record Record, cancel context.CancelFunc) *Owner {
	return &Owner{store: store, record: record, cancel: cancel}
}

func (o *Owner) Backend(backend string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.record.Containment = backend
	return o.store.Write(o.record)
}

func (o *Owner) Finish(runErr error, cleanup Cleanup, code *int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.record.Cleanup, o.record.ExitCode = cleanup, code
	o.record.Status = "completed"
	if runErr != nil {
		o.record.Status, o.record.Reason = "failed", "job execution failed; see private error log"
	}
	if o.record.CancelRequested {
		o.record.Status, o.record.Reason = statusCancelled, "cancellation requested by job ID"
	}
	return o.store.Write(o.record)
}

func (o *Owner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/cancel/"+o.record.JobID || r.URL.RawQuery != "" || r.ContentLength != 0 {
		http.Error(w, "unsupported job operation", http.StatusBadRequest)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.record.Terminal() {
		o.record.CancelRequested = true
		if err := o.store.Write(o.record); err != nil {
			http.Error(w, "could not persist cancellation request", http.StatusInternalServerError)
			return
		}
		o.cancel()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(o.record)
}

func (s Store) socketPath(id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	// TMPDIR can differ between launch and cancel, and long paths exceed sun_path.
	const socketRoot = "/tmp"
	root := filepath.Join(socketRoot, fmt.Sprintf("ccr-jobs-%d", os.Getuid()))
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("creating job socket directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("checking job socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("job socket directory must be a private directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("job socket directory must belong to the current user")
	}
	absRoot, err := filepath.Abs(s.Root)
	if err != nil {
		return "", fmt.Errorf("resolving job root: %w", err)
	}
	key := sha256.Sum256([]byte(absRoot + "/" + id))
	return filepath.Join(root, fmt.Sprintf("%x.sock", key[:16])), nil
}

func (o *Owner) Listen() (net.Listener, error) {
	path, err := o.store.socketPath(o.record.JobID)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("opening job control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("securing job control socket: %w", err)
	}
	return listener, nil
}

// Cancel requests cancellation; its response may still be running while stopping.
func (s Store) Cancel(ctx context.Context, id string) (Record, error) {
	r, err := s.Status(id)
	if err != nil || r.Terminal() {
		return r, err
	}
	path, err := s.socketPath(id)
	if err != nil {
		return Record{}, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/cancel/"+id, http.NoBody)
	if err != nil {
		return Record{}, fmt.Errorf("creating cancellation request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		latest, statusErr := s.Status(id)
		if statusErr == nil && latest.Terminal() {
			return latest, nil
		}
		return Record{}, fmt.Errorf("job owner unavailable; no signal fallback attempted: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Record{}, fmt.Errorf("job owner rejected cancellation (HTTP %d)", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&r); err != nil {
		return Record{}, fmt.Errorf("decoding cancellation response: %w", err)
	}
	r.Cleanup.Normalize()
	return r, nil
}
