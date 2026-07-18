package conformance

import (
	"net/http"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

const (
	StatusPassed        = "passed"
	StatusFailed        = "failed"
	StatusNotApplicable = "not_applicable"
)

type Config struct {
	Store      *store.Store
	Secrets    secret.Backend
	HTTPClient *http.Client
	Alias      string
	Timeout    time.Duration
	SmokeOnly  bool
}

type Check struct {
	Name     string
	Status   string
	Latency  time.Duration
	Evidence string
}

type Result struct {
	Alias         string
	ProviderName  string
	ProviderModel string
	Protocol      string
	Status        string
	StartedAt     time.Time
	CompletedAt   time.Time
	Checks        []Check
}

type target struct {
	model        store.Model
	provider     store.Provider
	capabilities providers.Capabilities
}
