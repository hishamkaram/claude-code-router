package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func TestBrowserExecutorCapturesScreenshotViaCDP(t *testing.T) {
	t.Parallel()

	server := newFakeCDPServer(t)
	runner := newRecordingRunner(newFakeProcess(true), newFakeProcess(false))
	executor, err := NewDockerBrowser(context.Background(), DockerBrowserOptions{
		Runner:        runner,
		DockerPath:    "docker",
		Image:         "example/browser:latest",
		BrowserBinary: "chromium",
		HostDebugPort: server.port(),
		LaunchID:      "launch_screen",
	})
	if err != nil {
		t.Fatalf("NewDockerBrowser() error = %v", err)
	}
	defer func() {
		if closeErr := executor.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	}()

	observation, err := executor.Execute(context.Background(), cua.Action{CallID: "call_1", Kind: cua.ActionScreenshot})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(observation.Screenshot) != "fake-png" || observation.ContentType != "image/png" {
		t.Fatalf("observation = %#v", observation)
	}
	if !server.hasMethod("Page.captureScreenshot") {
		t.Fatalf("Page.captureScreenshot was not called; methods = %#v", server.methods())
	}
}

type browserCDPActionCase struct {
	name       string
	action     cua.Action
	assertions func(*testing.T, *fakeCDPServer)
}

func TestExecuteBrowserCDPMouseActions(t *testing.T) {
	t.Parallel()

	tests := []browserCDPActionCase{
		{
			name: "right click from raw coordinates",
			action: cua.Action{
				CallID: "call_click",
				Kind:   cua.ActionClick,
				Raw:    json.RawMessage(`{"x":11,"y":12,"button":"right"}`),
			},
			assertions: func(t *testing.T, server *fakeCDPServer) {
				t.Helper()
				params := server.paramsForMethod("Input.dispatchMouseEvent")
				if len(params) != 3 {
					t.Fatalf("mouse events = %d, want 3", len(params))
				}
				if params[1]["type"] != "mousePressed" || params[1]["button"] != "right" ||
					params[1]["x"] != float64(11) || params[1]["y"] != float64(12) {
					t.Fatalf("pressed params = %#v", params[1])
				}
			},
		},
		{
			name:   "double click",
			action: cua.Action{CallID: "call_double", Kind: cua.ActionDoubleClick, X: 3, Y: 4},
			assertions: func(t *testing.T, server *fakeCDPServer) {
				t.Helper()
				params := server.paramsForMethod("Input.dispatchMouseEvent")
				if len(params) != 5 {
					t.Fatalf("mouse events = %d, want 5", len(params))
				}
				if params[3]["type"] != "mousePressed" || params[3]["clickCount"] != float64(2) {
					t.Fatalf("second press params = %#v", params[3])
				}
			},
		},
		{
			name: "drag",
			action: cua.Action{
				CallID: "call_drag",
				Kind:   cua.ActionDrag,
				Raw:    json.RawMessage(`{"path":[[1,2],{"x":3,"y":4},[5,6]]}`),
			},
			assertions: func(t *testing.T, server *fakeCDPServer) {
				t.Helper()
				got := server.mouseEventTypes()
				want := "mouseMoved,mousePressed,mouseMoved,mouseMoved,mouseReleased"
				if strings.Join(got, ",") != want {
					t.Fatalf("mouse event types = %#v, want %s", got, want)
				}
			},
		},
		{
			name:   "move",
			action: cua.Action{CallID: "call_move", Kind: cua.ActionMove, X: 7, Y: 8},
			assertions: func(t *testing.T, server *fakeCDPServer) {
				t.Helper()
				params := server.paramsForMethod("Input.dispatchMouseEvent")
				if len(params) != 1 || params[0]["type"] != "mouseMoved" ||
					params[0]["x"] != float64(7) || params[0]["y"] != float64(8) {
					t.Fatalf("move params = %#v", params)
				}
			},
		},
		{
			name: "scroll",
			action: cua.Action{
				CallID: "call_scroll",
				Kind:   cua.ActionScroll,
				Raw:    json.RawMessage(`{"x":9,"y":10,"scrollX":0,"scrollY":-200}`),
			},
			assertions: func(t *testing.T, server *fakeCDPServer) {
				t.Helper()
				params := server.paramsForMethod("Input.dispatchMouseEvent")
				if len(params) != 1 || params[0]["type"] != "mouseWheel" || params[0]["deltaY"] != float64(-200) {
					t.Fatalf("scroll params = %#v", params)
				}
			},
		},
	}

	runBrowserCDPActionCases(t, tests)
}

func TestExecuteBrowserCDPKeyboardActions(t *testing.T) {
	t.Parallel()

	tests := []browserCDPActionCase{
		{
			name:   "type",
			action: cua.Action{CallID: "call_type", Kind: cua.ActionType, Text: " \n\t"},
			assertions: func(t *testing.T, server *fakeCDPServer) {
				t.Helper()
				params := server.paramsForMethod("Input.insertText")
				if len(params) != 1 || params[0]["text"] != " \n\t" {
					t.Fatalf("insert text params = %#v", params)
				}
			},
		},
		{
			name:   "keypress",
			action: cua.Action{CallID: "call_key", Kind: cua.ActionKeypress, Keys: []string{"CTRL", "L"}},
			assertions: func(t *testing.T, server *fakeCDPServer) {
				t.Helper()
				params := server.paramsForMethod("Input.dispatchKeyEvent")
				if len(params) != 2 {
					t.Fatalf("key events = %d, want 2", len(params))
				}
				if params[0]["type"] != "keyDown" || params[0]["key"] != "L" || params[0]["modifiers"] != float64(cdpModifierCtrl) {
					t.Fatalf("keydown params = %#v", params[0])
				}
				if params[1]["type"] != "keyUp" || params[1]["key"] != "L" || params[1]["modifiers"] != float64(cdpModifierCtrl) {
					t.Fatalf("keyup params = %#v", params[1])
				}
			},
		},
	}

	runBrowserCDPActionCases(t, tests)
}

func runBrowserCDPActionCases(t *testing.T, tests []browserCDPActionCase) {
	t.Helper()

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := newFakeCDPServer(t)
			_, err := executeBrowserCDPAction(context.Background(), "local-browser", server.server.Client(), server.debugURL(), test.action)
			if err != nil {
				t.Fatalf("executeBrowserCDPAction() error = %v", err)
			}
			test.assertions(t, server)
		})
	}
}

func TestBrowserCDPDragReleasesMouseAfterMoveFailure(t *testing.T) {
	t.Parallel()

	server := newFakeCDPServer(t)
	server.failMouseEventNumber(4)
	_, err := executeBrowserCDPAction(context.Background(), "local-browser", server.server.Client(), server.debugURL(), cua.Action{
		CallID: "call_drag", Kind: cua.ActionDrag,
		Raw: json.RawMessage(`{"path":[[1,2],[3,4],[5,6]]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "Input.dispatchMouseEvent failed") {
		t.Fatalf("executeBrowserCDPAction() error = %v, want injected mouse failure", err)
	}
	params := server.paramsForMethod("Input.dispatchMouseEvent")
	if got, want := strings.Join(server.mouseEventTypes(), ","), "mouseMoved,mousePressed,mouseMoved,mouseMoved,mouseReleased"; got != want {
		t.Fatalf("mouse event types = %s, want %s", got, want)
	}
	release := params[len(params)-1]
	if release["type"] != "mouseReleased" || release["x"] != float64(3) || release["y"] != float64(4) {
		t.Fatalf("cleanup release params = %#v, want release at last successful drag point", release)
	}
}

func TestActionScrollDeltaMatchesValidationAliasPrecedence(t *testing.T) {
	t.Parallel()

	deltaX, deltaY := actionScrollDelta(json.RawMessage(`{
		"scroll_x": 1,
		"scroll_y": -2,
		"scrollX": 1000000,
		"scrollY": 1000000,
		"delta_x": 3,
		"delta_y": -4,
		"deltaX": 5,
		"deltaY": -6
	}`))
	if deltaX != 1 || deltaY != -2 {
		t.Fatalf("scroll deltas = (%d,%d), want (1,-2)", deltaX, deltaY)
	}

	deltaX, deltaY = actionScrollDelta(json.RawMessage(`{
		"delta_x": 3,
		"delta_y": -4,
		"deltaX": 1000000,
		"deltaY": 1000000
	}`))
	if deltaX != 3 || deltaY != -4 {
		t.Fatalf("delta aliases = (%d,%d), want (3,-4)", deltaX, deltaY)
	}
}

func TestBrowserCDPAppliesMouseModifiers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		action    cua.Action
		modifiers int
	}{
		{
			name:      "click",
			action:    cua.Action{CallID: "click", Kind: cua.ActionClick, X: 1, Y: 2, Keys: []string{"SHIFT"}},
			modifiers: cdpModifierShift,
		},
		{
			name: "drag",
			action: cua.Action{
				CallID: "drag", Kind: cua.ActionDrag, Keys: []string{"CTRL"},
				Raw: json.RawMessage(`{"path":[[1,2],[3,4]]}`),
			},
			modifiers: cdpModifierCtrl,
		},
		{
			name:      "move",
			action:    cua.Action{CallID: "move", Kind: cua.ActionMove, X: 1, Y: 2, Keys: []string{"META"}},
			modifiers: cdpModifierMeta,
		},
		{
			name: "scroll",
			action: cua.Action{
				CallID: "scroll", Kind: cua.ActionScroll, X: 1, Y: 2, Keys: []string{"ALT"},
				Raw: json.RawMessage(`{"x":1,"y":2,"scrollX":0,"scrollY":1}`),
			},
			modifiers: cdpModifierAlt,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := newFakeCDPServer(t)
			_, err := executeBrowserCDPAction(context.Background(), "local-browser", server.server.Client(), server.debugURL(), test.action)
			if err != nil {
				t.Fatalf("executeBrowserCDPAction() error = %v", err)
			}
			params := server.paramsForMethod("Input.dispatchMouseEvent")
			if len(params) == 0 {
				t.Fatal("mouse action made no CDP calls")
			}
			for _, values := range params {
				if got := values["modifiers"]; got != float64(test.modifiers) {
					t.Fatalf("mouse event modifiers = %#v, want %d", got, test.modifiers)
				}
			}
		})
	}
}

func TestBrowserCDPSupportsDragFromTo(t *testing.T) {
	t.Parallel()

	server := newFakeCDPServer(t)
	_, err := executeBrowserCDPAction(context.Background(), "local-browser", server.server.Client(), server.debugURL(), cua.Action{
		CallID: "call_drag", Kind: cua.ActionDrag,
		Raw: json.RawMessage(`{"type":"drag","from":{"x":1,"y":2},"to":{"x":3,"y":4}}`),
	})
	if err != nil {
		t.Fatalf("executeBrowserCDPAction() error = %v", err)
	}
	params := server.paramsForMethod("Input.dispatchMouseEvent")
	if len(params) != 4 || params[0]["type"] != "mouseMoved" || params[1]["type"] != "mousePressed" ||
		params[2]["type"] != "mouseMoved" || params[3]["type"] != "mouseReleased" ||
		params[0]["x"] != float64(1) || params[0]["y"] != float64(2) ||
		params[3]["x"] != float64(3) || params[3]["y"] != float64(4) {
		t.Fatalf("drag CDP params = %#v", params)
	}
}

func TestBrowserWaitActionDoesNotContactCDP(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("wait action unexpectedly contacted CDP endpoint")
		return nil, nil
	})}
	_, err := executeBrowserCDPAction(context.Background(), "local-browser", client, "http://127.0.0.1:45682/json/version", cua.Action{
		CallID: "call_wait",
		Kind:   cua.ActionWait,
		Raw:    json.RawMessage(`{"duration_ms":1}`),
	})
	if err != nil {
		t.Fatalf("executeBrowserCDPAction() error = %v", err)
	}
}

func TestBrowserCDPCreatesPageWhenNoTargetExists(t *testing.T) {
	t.Parallel()

	server := newFakeCDPServer(t)
	server.setTargets(nil)
	_, err := executeBrowserCDPAction(context.Background(), "local-browser", server.server.Client(), server.debugURL(), cua.Action{
		CallID: "call_screen",
		Kind:   cua.ActionScreenshot,
	})
	if err != nil {
		t.Fatalf("executeBrowserCDPAction() error = %v", err)
	}
	if !server.hasMethod("Target.createTarget") {
		t.Fatalf("Target.createTarget was not called; methods = %#v", server.methods())
	}
}

func TestBrowserCDPRejectsUnsafeWebSocketTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		target  string
		wantErr string
	}{
		{
			name:    "non loopback",
			target:  "ws://203.0.113.10:45682/devtools/browser/test",
			wantErr: "not loopback",
		},
		{
			name:    "port mismatch",
			target:  "ws://127.0.0.1:1/devtools/browser/test",
			wantErr: "debug endpoint port",
		},
		{
			name:    "credentials",
			target:  "ws://user:pass@127.0.0.1:45682/devtools/browser/test",
			wantErr: "credentials",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				body := fmt.Sprintf(`{"webSocketDebuggerUrl":%q}`, test.target)
				return textResponse(http.StatusOK, "application/json", body), nil
			})}
			_, err := executeBrowserCDPAction(context.Background(), "local-browser", client, "http://127.0.0.1:45682/json/version", cua.Action{
				CallID: "call_screen",
				Kind:   cua.ActionScreenshot,
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("executeBrowserCDPAction() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestBrowserCDPActionHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	_, err := executeBrowserCDPAction(ctx, "local-browser", client, "http://127.0.0.1:45682/json/version", cua.Action{
		CallID: "call_screen",
		Kind:   cua.ActionScreenshot,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeBrowserCDPAction() error = %v, want context.Canceled", err)
	}
}

type fakeCDPServer struct {
	server *httptest.Server

	mu          sync.Mutex
	requests    []cdpRequest
	targetInfos []map[string]any
	failMouse   int
}

func newFakeCDPServer(t *testing.T) *fakeCDPServer {
	t.Helper()

	fake := &fakeCDPServer{
		targetInfos: []map[string]any{{"targetId": "page-1", "type": "page"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", fake.handleVersion)
	mux.HandleFunc("/devtools/browser/test", fake.handleWebSocket)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	fake.server = server
	return fake
}

func (server *fakeCDPServer) debugURL() string {
	return server.server.URL + "/json/version"
}

func (server *fakeCDPServer) port() int {
	parsed, err := url.Parse(server.server.URL)
	if err != nil {
		panic(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		panic(err)
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil {
		panic(err)
	}
	return parsedPort
}

func (server *fakeCDPServer) handleVersion(w http.ResponseWriter, _ *http.Request) {
	websocketURL := "ws" + strings.TrimPrefix(server.server.URL, "http") + "/devtools/browser/test"
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":%q}`, websocketURL)
}

func (server *fakeCDPServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()
	for {
		messageType, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var request cdpRequest
		if decodeErr := json.Unmarshal(data, &request); decodeErr != nil {
			return
		}
		server.record(request)
		response, err := json.Marshal(server.responseFor(request))
		if err != nil {
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, response); err != nil {
			return
		}
	}
}

func (server *fakeCDPServer) responseFor(request cdpRequest) map[string]any {
	if request.Method == "Input.dispatchMouseEvent" && server.shouldFailMouseEvent() {
		return map[string]any{"id": request.ID, "error": map[string]any{"code": -32000}}
	}
	response := map[string]any{"id": request.ID, "result": map[string]any{}}
	switch request.Method {
	case "Target.getTargets":
		response["result"] = map[string]any{"targetInfos": server.targets()}
	case "Target.createTarget":
		response["result"] = map[string]any{"targetId": "page-created"}
	case "Target.attachToTarget":
		response["result"] = map[string]any{"sessionId": "session-1"}
	case "Page.captureScreenshot":
		response["result"] = map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("fake-png"))}
	}
	return response
}

func (server *fakeCDPServer) failMouseEventNumber(number int) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.failMouse = number
}

func (server *fakeCDPServer) shouldFailMouseEvent() bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.failMouse <= 0 {
		return false
	}
	count := 0
	for _, recorded := range server.requests {
		if recorded.Method == "Input.dispatchMouseEvent" {
			count++
		}
	}
	return count == server.failMouse
}

func (server *fakeCDPServer) record(request cdpRequest) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.requests = append(server.requests, request)
}

func (server *fakeCDPServer) setTargets(targets []map[string]any) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.targetInfos = targets
}

func (server *fakeCDPServer) targets() []map[string]any {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]map[string]any(nil), server.targetInfos...)
}

func (server *fakeCDPServer) methods() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	methods := make([]string, 0, len(server.requests))
	for _, request := range server.requests {
		methods = append(methods, request.Method)
	}
	return methods
}

func (server *fakeCDPServer) hasMethod(method string) bool {
	for _, recorded := range server.methods() {
		if recorded == method {
			return true
		}
	}
	return false
}

func (server *fakeCDPServer) paramsForMethod(method string) []map[string]any {
	server.mu.Lock()
	defer server.mu.Unlock()
	var params []map[string]any
	for _, request := range server.requests {
		if request.Method != method {
			continue
		}
		paramMap, ok := request.Params.(map[string]any)
		if !ok {
			continue
		}
		params = append(params, paramMap)
	}
	return params
}

func (server *fakeCDPServer) mouseEventTypes() []string {
	params := server.paramsForMethod("Input.dispatchMouseEvent")
	types := make([]string, 0, len(params))
	for _, param := range params {
		eventType, _ := param["type"].(string)
		types = append(types, eventType)
	}
	return types
}
