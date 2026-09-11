package jobs

const (
	DispositionNotStarted = "not_started"
	DispositionStopped    = "stopped"
	DispositionUnknown    = "unknown"

	ReasonStartupFailed           = "startup_failed"
	ReasonExecutionFailed         = "execution_failed"
	ReasonObservationFailed       = "observation_failed"
	ReasonSessionIdentityMismatch = "session_identity_mismatch"
	ReasonOwnerLost               = "owner_lost"
	ReasonAdmissionInterrupted    = "admission_interrupted"
)

// ResultEvidence commits a byte prefix of direct stdout. Readers must verify
// its digest before interpreting those bytes; subsequent appends are excluded.
type ResultEvidence struct {
	Boundary   int64  `json:"boundary"`
	SHA256     string `json:"sha256"`
	SessionID  string `json:"session_id"`
	Model      string `json:"model"`
	Successful bool   `json:"successful"`
}
