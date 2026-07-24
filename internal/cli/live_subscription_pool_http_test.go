//go:build live

package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

type liveSubscriptionHTTPLauncher struct {
	mu     sync.Mutex
	starts []liveSubscriptionStart
}

type liveSubscriptionStart struct {
	args       []string
	gatewayURL string
	headers    string
	oauthToken string
}

func (s liveSubscriptionStart) UsesToken(token string) bool {
	return s.oauthToken == token
}

func (l *liveSubscriptionHTTPLauncher) Start(
	ctx context.Context,
	args []string,
	env ClaudeEnvironment,
	_ io.Reader,
	_, _ io.Writer,
) (ClaudeProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start := liveSubscriptionStart{
		args:       append([]string(nil), args...),
		gatewayURL: environmentEntries(env.Set)["ANTHROPIC_BASE_URL"],
		headers:    environmentEntries(env.Set)["ANTHROPIC_CUSTOM_HEADERS"],
		oauthToken: environmentEntries(env.Set)["CLAUDE_CODE_OAUTH_TOKEN"],
	}
	l.mu.Lock()
	l.starts = append(l.starts, start)
	pid := 7000 + len(l.starts)
	l.mu.Unlock()
	processCtx, cancel := context.WithCancel(ctx)
	process := &liveSubscriptionHTTPProcess{pid: pid, cancel: cancel, done: make(chan error, 1)}
	go func() {
		process.done <- postLiveSubscriptionGatewayRequest(processCtx, start)
	}()
	return process, nil
}

func (l *liveSubscriptionHTTPLauncher) StartCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.starts)
}

func (l *liveSubscriptionHTTPLauncher) StartAt(index int) liveSubscriptionStart {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts[index]
}

type liveSubscriptionHTTPProcess struct {
	pid    int
	cancel context.CancelFunc
	done   chan error
}

func (p *liveSubscriptionHTTPProcess) PID() int {
	return p.pid
}

func (p *liveSubscriptionHTTPProcess) Done() <-chan error {
	return p.done
}

func (p *liveSubscriptionHTTPProcess) Stop() error {
	p.cancel()
	return nil
}

func postLiveSubscriptionGatewayRequest(ctx context.Context, start liveSubscriptionStart) error {
	body := `{"model":"sonnet","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, start.gatewayURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+start.oauthToken)
	for _, line := range strings.Split(start.headers, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if ok {
			req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices) {
		return nil
	}
	return fmt.Errorf("gateway fixture status %d", resp.StatusCode)
}
