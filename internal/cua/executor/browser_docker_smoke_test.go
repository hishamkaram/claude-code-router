package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func TestDockerBrowserSmoke(t *testing.T) {
	if os.Getenv("CCR_CUA_DOCKER_SMOKE") != "1" {
		t.Skip("set CCR_CUA_DOCKER_SMOKE=1 to run the Docker browser fixture")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("Docker browser fixture requires docker on PATH: %v", err)
	}
	image := strings.TrimSpace(os.Getenv("CCR_CUA_DOCKER_IMAGE"))
	if image == "" {
		t.Fatal("Docker browser fixture requires CCR_CUA_DOCKER_IMAGE")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	browser, err := NewDockerBrowser(ctx, DockerBrowserOptions{
		Image:    image,
		LaunchID: "fixture-browser",
	})
	if err != nil {
		t.Fatalf("NewDockerBrowser() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := browser.Close(); closeErr != nil {
			t.Errorf("Docker browser Close() error = %v", closeErr)
		}
	})

	if waitErr := waitForDockerBrowser(ctx, browser); waitErr != nil {
		t.Fatal(waitErr)
	}
	observation, err := browser.Execute(ctx, cua.Action{CallID: "fixture-screenshot", Kind: cua.ActionScreenshot})
	if err != nil {
		t.Fatalf("Docker browser screenshot error = %v", err)
	}
	if observation.ContentType != "image/png" || len(observation.Screenshot) == 0 {
		t.Fatalf("Docker browser screenshot = %#v, want non-empty PNG", observation)
	}
}

func waitForDockerBrowser(ctx context.Context, browser *DockerBrowser) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var checkErr error
	for {
		if checkErr = browser.Check(ctx); checkErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Docker browser did not become ready: %w", errors.Join(ctx.Err(), checkErr))
		case <-ticker.C:
		}
	}
}
