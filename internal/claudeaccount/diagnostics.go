package claudeaccount

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	claudeOAuthAPIBaseURL       = "https://api.anthropic.com"
	maxDiagnosticsResponseBytes = 1 << 20
)

type QuotaWindow struct {
	Kind        string
	Utilization float64
	ResetsAt    time.Time
}

type AccountDiagnostics struct {
	IdentityFingerprint string
	Plan                string
	Windows             []QuotaWindow
}

type DiagnosticsClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewDiagnosticsClient(httpClient *http.Client) DiagnosticsClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return DiagnosticsClient{baseURL: claudeOAuthAPIBaseURL, httpClient: &client}
}

func (c DiagnosticsClient) Probe(ctx context.Context, token string) (AccountDiagnostics, error) {
	token, err := ValidateToken(token)
	if err != nil {
		return AccountDiagnostics{}, fmt.Errorf("validating Claude account credential: %w", err)
	}
	var profile oauthProfileDocument
	if profileErr := c.getJSON(ctx, token, "/api/oauth/profile", &profile); profileErr != nil {
		return AccountDiagnostics{}, fmt.Errorf("fetching Claude account profile: %w", profileErr)
	}
	var usage oauthUsageDocument
	if usageErr := c.getJSON(ctx, token, "/api/oauth/usage", &usage); usageErr != nil {
		return AccountDiagnostics{}, fmt.Errorf("fetching Claude account usage: %w", usageErr)
	}
	fingerprint, err := profile.identityFingerprint()
	if err != nil {
		return AccountDiagnostics{}, err
	}
	windows, err := usage.quotaWindows()
	if err != nil {
		return AccountDiagnostics{}, err
	}
	return AccountDiagnostics{
		IdentityFingerprint: fingerprint,
		Plan:                profile.plan(),
		Windows:             windows,
	}, nil
}

func (c DiagnosticsClient) getJSON(ctx context.Context, token, path string, target any) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, strings.TrimRight(c.baseURL, "/")+path, http.NoBody,
	)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("User-Agent", "ccr-account-diagnostics")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("service returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDiagnosticsResponseBytes+1))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if len(raw) > maxDiagnosticsResponseBytes {
		return fmt.Errorf("response exceeded %d bytes", maxDiagnosticsResponseBytes)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decoding response JSON: %w", err)
	}
	return nil
}

type oauthProfileDocument struct {
	Account struct {
		UUID         string `json:"uuid"`
		HasClaudeMax bool   `json:"has_claude_max"`
		HasClaudePro bool   `json:"has_claude_pro"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
	} `json:"organization"`
}

func (p oauthProfileDocument) identityFingerprint() (string, error) {
	if strings.TrimSpace(p.Account.UUID) == "" || strings.TrimSpace(p.Organization.UUID) == "" {
		return "", fmt.Errorf("claude account profile omitted stable identity fields")
	}
	sum := sha256.Sum256([]byte(p.Account.UUID + "\x00" + p.Organization.UUID))
	return hex.EncodeToString(sum[:8]), nil
}

func (p oauthProfileDocument) plan() string {
	switch {
	case p.Account.HasClaudeMax:
		return "max"
	case p.Account.HasClaudePro:
		return "pro"
	default:
		return "other"
	}
}

type oauthUsageDocument struct {
	FiveHour       *oauthQuotaWindow `json:"five_hour"`
	SevenDay       *oauthQuotaWindow `json:"seven_day"`
	SevenDayOAuth  *oauthQuotaWindow `json:"seven_day_oauth_apps"`
	SevenDayOpus   *oauthQuotaWindow `json:"seven_day_opus"`
	SevenDaySonnet *oauthQuotaWindow `json:"seven_day_sonnet"`
}

type oauthQuotaWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    *string `json:"resets_at"`
}

func (d oauthUsageDocument) quotaWindows() ([]QuotaWindow, error) {
	candidates := []struct {
		kind   string
		window *oauthQuotaWindow
	}{
		{kind: "five_hour", window: d.FiveHour},
		{kind: "seven_day", window: d.SevenDay},
		{kind: "seven_day_oauth_apps", window: d.SevenDayOAuth},
		{kind: "seven_day_opus", window: d.SevenDayOpus},
		{kind: "seven_day_sonnet", window: d.SevenDaySonnet},
	}
	windows := make([]QuotaWindow, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.window == nil {
			continue
		}
		if candidate.window.Utilization < 0 || candidate.window.Utilization > 100 {
			return nil, fmt.Errorf("claude account usage returned invalid %s utilization", candidate.kind)
		}
		var resetAt time.Time
		if candidate.window.ResetsAt != nil && strings.TrimSpace(*candidate.window.ResetsAt) != "" {
			var err error
			resetAt, err = time.Parse(time.RFC3339Nano, *candidate.window.ResetsAt)
			if err != nil {
				return nil, fmt.Errorf("claude account usage returned invalid %s reset", candidate.kind)
			}
		}
		windows = append(windows, QuotaWindow{
			Kind: candidate.kind, Utilization: candidate.window.Utilization, ResetsAt: resetAt,
		})
	}
	return windows, nil
}
