package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

const (
	maxBrowserScreenshotBytes     = cua.MaxComputerScreenshotBytes
	cdpMouseReleaseCleanupTimeout = time.Second
)

func executeBrowserCDPAction(ctx context.Context, executorName string, client *http.Client, debugEndpoint string, action cua.Action) (cua.Observation, error) {
	if ctx == nil {
		return cua.Observation{}, fmt.Errorf("browser action context is required")
	}
	if err := action.Validate(); err != nil {
		return cua.Observation{}, err
	}
	if action.Kind == cua.ActionWait {
		return waitBrowserAction(ctx, executorName, action)
	}
	actionCtx, cancel := browserActionContext(ctx)
	defer cancel()
	cdp, err := newCDPClient(actionCtx, client, debugEndpoint)
	if err != nil {
		return cua.Observation{}, fmt.Errorf("connecting %s browser CDP: %w", executorName, err)
	}
	defer func() {
		_ = cdp.close()
	}()
	sessionID, err := cdp.attachPage(actionCtx)
	if err != nil {
		return cua.Observation{}, fmt.Errorf("attaching %s browser page CDP session: %w", executorName, err)
	}
	observation, err := executeCDPPageAction(actionCtx, cdp, sessionID, executorName, action)
	if err != nil {
		return cua.Observation{}, fmt.Errorf("executing %s browser CDP action %q: %w", executorName, action.Kind, err)
	}
	return observation, nil
}

func browserActionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, defaultBrowserActionTimeout)
}

func waitBrowserAction(ctx context.Context, executorName string, action cua.Action) (cua.Observation, error) {
	timer := time.NewTimer(waitDuration(action.Raw))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return cua.Observation{}, fmt.Errorf("waiting in %s executor: %w", executorName, ctx.Err())
	case <-timer.C:
		return cua.Observation{}, nil
	}
}

func executeCDPPageAction(ctx context.Context, client *cdpClient, sessionID, executorName string, action cua.Action) (cua.Observation, error) {
	switch action.Kind {
	case cua.ActionScreenshot:
		return captureCDPScreenshot(ctx, client, sessionID)
	case cua.ActionClick:
		return cua.Observation{}, dispatchCDPClick(ctx, client, sessionID, action, 1)
	case cua.ActionDoubleClick:
		return cua.Observation{}, dispatchCDPClick(ctx, client, sessionID, action, 2)
	case cua.ActionDrag:
		return cua.Observation{}, dispatchCDPDrag(ctx, client, sessionID, action)
	case cua.ActionMove:
		return cua.Observation{}, dispatchCDPMove(ctx, client, sessionID, action)
	case cua.ActionType:
		return cua.Observation{}, dispatchCDPInsertText(ctx, client, sessionID, action)
	case cua.ActionKeypress:
		return cua.Observation{}, dispatchCDPKeypress(ctx, client, sessionID, action)
	case cua.ActionScroll:
		return cua.Observation{}, dispatchCDPScroll(ctx, client, sessionID, action)
	default:
		return cua.Observation{}, unsupportedAction(executorName, action.Kind, "browser CDP action mapping is not implemented")
	}
}

func captureCDPScreenshot(ctx context.Context, client *cdpClient, sessionID string) (cua.Observation, error) {
	var result struct {
		Data string `json:"data"`
	}
	if err := client.call(ctx, "Page.captureScreenshot", map[string]any{
		"format":      "png",
		"fromSurface": true,
	}, sessionID, &result); err != nil {
		return cua.Observation{}, err
	}
	if base64.StdEncoding.DecodedLen(len(result.Data)) > maxBrowserScreenshotBytes {
		return cua.Observation{}, fmt.Errorf("browser screenshot exceeds the 32 MiB limit")
	}
	data, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		return cua.Observation{}, fmt.Errorf("decoding browser screenshot: %w", err)
	}
	return cua.Observation{Screenshot: data, ContentType: "image/png"}, nil
}

func dispatchCDPClick(ctx context.Context, client *cdpClient, sessionID string, action cua.Action, clickCount int) error {
	point, err := actionPoint(action)
	if err != nil {
		return err
	}
	modifiers, err := cdpMouseModifiers(action.Keys)
	if err != nil {
		return err
	}
	button, err := actionMouseButton(action.Raw)
	if err != nil {
		return err
	}
	if clickCount == 2 && button != "left" {
		return fmt.Errorf("double click only supports the left mouse button")
	}
	if err := dispatchCDPMouse(ctx, client, sessionID, cdpMouseEventParams{
		Type: "mouseMoved", X: point.X, Y: point.Y, Button: "none", Modifiers: modifiers,
	}); err != nil {
		return err
	}
	for index := 1; index <= clickCount; index++ {
		if err := dispatchCDPMouse(ctx, client, sessionID, cdpMouseEventParams{
			Type: "mousePressed", X: point.X, Y: point.Y, Button: button,
			Buttons: mouseButtonMask(button), ClickCount: index, Modifiers: modifiers,
		}); err != nil {
			return err
		}
		if err := dispatchCDPMouse(ctx, client, sessionID, cdpMouseEventParams{
			Type: "mouseReleased", X: point.X, Y: point.Y, Button: button, ClickCount: index, Modifiers: modifiers,
		}); err != nil {
			return err
		}
	}
	return nil
}

func dispatchCDPDrag(ctx context.Context, client *cdpClient, sessionID string, action cua.Action) (resultErr error) {
	path, err := actionDragPath(action)
	if err != nil {
		return err
	}
	modifiers, err := cdpMouseModifiers(action.Keys)
	if err != nil {
		return err
	}
	start := path[0]
	if err := dispatchCDPMouse(ctx, client, sessionID, cdpMouseEventParams{
		Type: "mouseMoved", X: start.X, Y: start.Y, Button: "none", Modifiers: modifiers,
	}); err != nil {
		return err
	}
	if err := dispatchCDPMouse(ctx, client, sessionID, cdpMouseEventParams{
		Type: "mousePressed", X: start.X, Y: start.Y, Button: "left", Buttons: mouseButtonMask("left"), ClickCount: 1, Modifiers: modifiers,
	}); err != nil {
		return err
	}
	pressed := true
	releasePoint := start
	defer func() {
		if !pressed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cdpMouseReleaseCleanupTimeout)
		defer cancel()
		_ = dispatchCDPMouse(cleanupCtx, client, sessionID, cdpMouseEventParams{
			Type: "mouseReleased", X: releasePoint.X, Y: releasePoint.Y, Button: "left", ClickCount: 1, Modifiers: modifiers,
		})
	}()
	for _, point := range path[1:] {
		if err := dispatchCDPMouse(ctx, client, sessionID, cdpMouseEventParams{
			Type: "mouseMoved", X: point.X, Y: point.Y, Button: "left", Buttons: mouseButtonMask("left"), Modifiers: modifiers,
		}); err != nil {
			return err
		}
		releasePoint = point
	}
	end := path[len(path)-1]
	if err := dispatchCDPMouse(ctx, client, sessionID, cdpMouseEventParams{
		Type: "mouseReleased", X: end.X, Y: end.Y, Button: "left", ClickCount: 1, Modifiers: modifiers,
	}); err != nil {
		return err
	}
	pressed = false
	return nil
}

func dispatchCDPMove(ctx context.Context, client *cdpClient, sessionID string, action cua.Action) error {
	point, err := actionPoint(action)
	if err != nil {
		return err
	}
	modifiers, err := cdpMouseModifiers(action.Keys)
	if err != nil {
		return err
	}
	return dispatchCDPMouse(ctx, client, sessionID, cdpMouseEventParams{
		Type: "mouseMoved", X: point.X, Y: point.Y, Button: "none", Modifiers: modifiers,
	})
}

func dispatchCDPInsertText(ctx context.Context, client *cdpClient, sessionID string, action cua.Action) error {
	text := actionText(action)
	if text == "" {
		return fmt.Errorf("type action requires non-empty text")
	}
	return client.call(ctx, "Input.insertText", map[string]any{"text": text}, sessionID, nil)
}

func dispatchCDPKeypress(ctx context.Context, client *cdpClient, sessionID string, action cua.Action) error {
	keys := actionKeys(action)
	if len(keys) == 0 {
		return fmt.Errorf("keypress action requires at least one key")
	}
	stroke, err := parseCDPKeyStroke(strings.Join(keys, "+"))
	if err != nil {
		return err
	}
	if err := client.call(ctx, "Input.dispatchKeyEvent", stroke.params("keyDown"), sessionID, nil); err != nil {
		return err
	}
	if err := client.call(ctx, "Input.dispatchKeyEvent", stroke.params("keyUp"), sessionID, nil); err != nil {
		return err
	}
	return nil
}

func dispatchCDPScroll(ctx context.Context, client *cdpClient, sessionID string, action cua.Action) error {
	point, err := actionPoint(action)
	if err != nil {
		return err
	}
	modifiers, err := cdpMouseModifiers(action.Keys)
	if err != nil {
		return err
	}
	deltaX, deltaY := actionScrollDelta(action.Raw)
	return dispatchCDPMouse(ctx, client, sessionID, cdpMouseEventParams{
		Type: "mouseWheel", X: point.X, Y: point.Y, Button: "none", DeltaX: deltaX, DeltaY: deltaY, Modifiers: modifiers,
	})
}

func dispatchCDPMouse(ctx context.Context, client *cdpClient, sessionID string, params cdpMouseEventParams) error {
	return client.call(ctx, "Input.dispatchMouseEvent", params, sessionID, nil)
}

type cdpMouseEventParams struct {
	Type       string `json:"type"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	Button     string `json:"button,omitempty"`
	Buttons    int    `json:"buttons,omitempty"`
	ClickCount int    `json:"clickCount,omitempty"`
	DeltaX     int    `json:"deltaX,omitempty"`
	DeltaY     int    `json:"deltaY,omitempty"`
	Modifiers  int    `json:"modifiers,omitempty"`
}

type cdpPoint struct {
	X int
	Y int
}

func actionPoint(action cua.Action) (cdpPoint, error) {
	if point, ok := rawPoint(action.Raw); ok {
		return point, nil
	}
	if err := (cua.Action{CallID: action.CallID, Kind: cua.ActionMove, X: action.X, Y: action.Y}).Validate(); err != nil {
		return cdpPoint{}, err
	}
	return cdpPoint{X: action.X, Y: action.Y}, nil
}

func actionMouseButton(raw json.RawMessage) (string, error) {
	button := rawString(raw, "button")
	if button == "" {
		return "left", nil
	}
	switch strings.ToLower(button) {
	case "left", "right", "middle":
		return strings.ToLower(button), nil
	default:
		return "", fmt.Errorf("mouse button %q is not supported", button)
	}
}

func mouseButtonMask(button string) int {
	switch button {
	case "left":
		return 1
	case "right":
		return 2
	case "middle":
		return 4
	default:
		return 0
	}
}

func actionDragPath(action cua.Action) ([]cdpPoint, error) {
	fields := rawFields(action.Raw)
	rawPath, hasPath := fields["path"]
	rawFrom, hasFrom := fields["from"]
	rawTo, hasTo := fields["to"]
	if hasPath && (hasFrom || hasTo) {
		return nil, fmt.Errorf("drag action must use path or from/to, not both")
	}
	if hasPath {
		return decodeCDPDragPath(rawPath)
	}
	if hasFrom != hasTo {
		return nil, fmt.Errorf("drag action requires both from and to points")
	}
	if !hasFrom {
		return nil, fmt.Errorf("drag action requires a path or from/to points")
	}
	from, err := decodeDragPoint(rawFrom)
	if err != nil {
		return nil, fmt.Errorf("decoding drag source: %w", err)
	}
	to, err := decodeDragPoint(rawTo)
	if err != nil {
		return nil, fmt.Errorf("decoding drag destination: %w", err)
	}
	return []cdpPoint{from, to}, nil
}

func decodeCDPDragPath(rawPath json.RawMessage) ([]cdpPoint, error) {
	var rawPoints []json.RawMessage
	if err := json.Unmarshal(rawPath, &rawPoints); err != nil {
		return nil, fmt.Errorf("decoding drag path: %w", err)
	}
	if len(rawPoints) < 2 {
		return nil, fmt.Errorf("drag action requires at least two path points")
	}
	points := make([]cdpPoint, 0, len(rawPoints))
	for _, rawPoint := range rawPoints {
		point, err := decodeDragPoint(rawPoint)
		if err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, nil
}

func decodeDragPoint(raw json.RawMessage) (cdpPoint, error) {
	var object map[string]float64
	if err := json.Unmarshal(raw, &object); err == nil {
		x, hasX := object["x"]
		y, hasY := object["y"]
		if hasX && hasY {
			return nonNegativePoint(x, y)
		}
	}
	var array []float64
	if err := json.Unmarshal(raw, &array); err != nil || len(array) < 2 {
		return cdpPoint{}, fmt.Errorf("drag path point must include x and y")
	}
	return nonNegativePoint(array[0], array[1])
}

func nonNegativePoint(x, y float64) (cdpPoint, error) {
	point := cdpPoint{X: roundedFloat(x), Y: roundedFloat(y)}
	if point.X < 0 || point.Y < 0 {
		return cdpPoint{}, fmt.Errorf("coordinates must be non-negative")
	}
	return point, nil
}

func actionText(action cua.Action) string {
	if action.Text != "" {
		return action.Text
	}
	value, _ := rawUntrimmedString(action.Raw, "text")
	return value
}

func actionKeys(action cua.Action) []string {
	if len(action.Keys) != 0 {
		return append([]string(nil), action.Keys...)
	}
	if values, ok := rawStringArray(action.Raw, "keys"); ok {
		return values
	}
	if key := rawString(action.Raw, "key"); key != "" {
		return []string{key}
	}
	return nil
}

func actionScrollDelta(raw json.RawMessage) (deltaX, deltaY int) {
	for _, names := range [][2]string{{"scroll_x", "scroll_y"}, {"scrollX", "scrollY"}, {"delta_x", "delta_y"}, {"deltaX", "deltaY"}} {
		floatX, hasX := rawNumber(raw, names[0])
		floatY, hasY := rawNumber(raw, names[1])
		if hasX && hasY {
			return roundedFloat(floatX), roundedFloat(floatY)
		}
	}
	return 0, 100
}

func rawPoint(raw json.RawMessage) (cdpPoint, bool) {
	x, hasX := rawNumber(raw, "x")
	y, hasY := rawNumber(raw, "y")
	if !hasX || !hasY {
		return cdpPoint{}, false
	}
	point, err := nonNegativePoint(x, y)
	return point, err == nil
}

func rawString(raw json.RawMessage, key string) string {
	decoded, ok := rawUntrimmedString(raw, key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(decoded)
}

func rawUntrimmedString(raw json.RawMessage, key string) (string, bool) {
	fields := rawFields(raw)
	value, ok := fields[key]
	if !ok {
		return "", false
	}
	var decoded string
	if json.Unmarshal(value, &decoded) != nil {
		return "", false
	}
	return decoded, true
}

func rawStringArray(raw json.RawMessage, key string) ([]string, bool) {
	fields := rawFields(raw)
	value, ok := fields[key]
	if !ok {
		return nil, false
	}
	var decoded []string
	if json.Unmarshal(value, &decoded) == nil {
		return decoded, true
	}
	var single string
	if json.Unmarshal(value, &single) == nil && strings.TrimSpace(single) != "" {
		return []string{strings.TrimSpace(single)}, true
	}
	return nil, false
}

func rawNumber(raw json.RawMessage, key string) (float64, bool) {
	fields := rawFields(raw)
	value, ok := fields[key]
	if !ok {
		return 0, false
	}
	var decoded float64
	if json.Unmarshal(value, &decoded) != nil {
		return 0, false
	}
	return decoded, true
}

func rawFields(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

func roundedFloat(value float64) int {
	return int(math.Round(value))
}
