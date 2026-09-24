package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/agentinput"
)

const (
	agentChildReservationTTL       = 30 * time.Second
	agentChildReservationRetention = 5 * time.Minute
	// Active child leases are refreshed whenever a child continuation is
	// accepted. The bound protects launches that disable lifecycle hooks or
	// terminate without delivering SessionEnd; expiry remains fail-closed.
	agentChildActiveLeaseTTL = 24 * time.Hour
)

var errAgentChildSessionCorrelation = errors.New("CCR routing error: provider emitted child work without Claude Code session correlation; the child was not started")

type agentChildDescriptor struct {
	model       string
	prompt      string
	description string
	workflow    bool
}

type pendingAgentChild struct {
	id                   uint64
	descriptor           agentChildDescriptor
	spawnAlias           string
	continuationIdentity string
	activationGeneration uint64
	active               bool
	expiresAt            time.Time
	rejectUntil          time.Time
}

func countAgentToolCalls(tools []openAIToolCall) int {
	count := 0
	for _, tool := range tools {
		if isAgentChildToolName(tool.Function.Name) {
			count++
		}
	}
	return count
}

func agentChildRequestMatches(req anthropicRequest, descriptor agentChildDescriptor) bool {
	// A parent continuation can contain the Agent/Task tool transcript that
	// created this reservation and then repeat the child's prompt in its latest
	// user turn. Prompt text alone is not enough to reclassify that request as
	// the child; only the native child envelope can disambiguate it.
	if !agentChildRequestHasChildEnvelope(req) && agentChildRequestHasAgentToolTranscript(req) {
		return false
	}
	if descriptor.workflow && !agentChildRequestHasChildEnvelope(req) {
		return false
	}
	return agentChildRequestTextMatches(req, descriptor)
}

func agentChildRequestTextMatches(req anthropicRequest, descriptor agentChildDescriptor) bool {
	if strings.TrimSpace(descriptor.prompt) == "" && strings.TrimSpace(descriptor.description) == "" {
		return !descriptor.workflow || agentChildRequestHasChildEnvelope(req)
	}
	segments := agentChildRequestSegments(req)
	for _, candidate := range []string{descriptor.prompt, descriptor.description} {
		candidate = normalizedAgentChildText(candidate)
		if candidate == "" {
			continue
		}
		for _, segment := range segments {
			if segment == candidate {
				return true
			}
		}
	}
	return false
}

func hasUnmarkedWorkflowChildEvidence(entries []pendingAgentChild, req anthropicRequest) bool {
	if agentChildRequestHasChildEnvelope(req) {
		return false
	}
	for index := range entries {
		descriptor := entries[index].descriptor
		if !descriptor.workflow {
			continue
		}
		// A dynamic workflow has no stable prompt to correlate. While its
		// reservation is live, an unmarked first-party request is ambiguous and
		// must fail closed instead of being treated as a normal parent request.
		if strings.TrimSpace(descriptor.prompt) == "" && strings.TrimSpace(descriptor.description) == "" {
			return true
		}
		if agentChildRequestTextMatches(req, descriptor) {
			return true
		}
	}
	return false
}

func agentChildContinuationMatches(req anthropicRequest, entry pendingAgentChild) bool {
	identity := agentChildContinuationIdentity(req, entry.descriptor)
	return identity != "" && identity == entry.continuationIdentity
}

func hasAgentChildContinuationEvidence(entries []pendingAgentChild, req anthropicRequest) bool {
	if agentChildRequestHasSubagentSystemMarker(req) {
		return true
	}
	// A parent transcript is evidence against consuming a prompt-only child
	// reservation. Dynamic workflows still fail closed when there is no such
	// parent evidence, below.
	if agentChildRequestHasAgentToolTranscript(req) {
		return false
	}
	if hasUnmarkedWorkflowChildEvidence(entries, req) {
		return true
	}
	return hasMarkerlessAgentChildEvidence(entries, req) || hasHistoricalAgentChildEvidence(entries, req)
}

func hasMarkerlessAgentChildEvidence(entries []pendingAgentChild, req anthropicRequest) bool {
	// A markerless child has no continuation identity once its original prompt
	// is compacted away. A normal Claude request always carries at least one
	// message; with an active markerless lease and no parent tool transcript,
	// refusing the request is the only safe way to prevent it from switching to
	// the Anthropic subscription. The empty-message shape is reserved for an
	// explicit top-level model probe and is handled by ordinary routing.
	if len(req.Messages) > 0 {
		for index := range entries {
			if entries[index].active && entries[index].continuationIdentity == "" {
				return true
			}
		}
	}
	return false
}

func hasHistoricalAgentChildEvidence(entries []pendingAgentChild, req anthropicRequest) bool {
	for index := range entries {
		entry := entries[index]
		if strings.TrimSpace(entry.descriptor.prompt) != "" || strings.TrimSpace(entry.descriptor.description) != "" {
			if agentChildRequestMatches(req, entry.descriptor) {
				return true
			}
		}
		for _, candidate := range []string{entry.descriptor.prompt, entry.descriptor.description} {
			if candidate != "" && agentChildRequestContainsHistoricalUserText(req, candidate) {
				return true
			}
		}
	}
	return false
}

func agentChildRequestContainsHistoricalUserText(req anthropicRequest, want string) bool {
	want = normalizedAgentChildText(want)
	if want == "" {
		return false
	}
	lastUser := -1
	for index := range req.Messages {
		if strings.EqualFold(strings.TrimSpace(req.Messages[index].Role), "user") {
			lastUser = index
		}
	}
	for index := range req.Messages {
		if index == lastUser || !strings.EqualFold(strings.TrimSpace(req.Messages[index].Role), "user") {
			continue
		}
		for _, segment := range visibleAgentChildSegments(req.Messages[index].Content) {
			if segment == want {
				return true
			}
		}
	}
	return false
}

func agentChildRequestHasAgentToolTranscript(req anthropicRequest) bool {
	for index := range req.Messages {
		message := req.Messages[index]
		if !strings.EqualFold(strings.TrimSpace(message.Role), "assistant") {
			continue
		}
		if contentBlocksContainChildSpawnTool(message.Content) {
			return true
		}
	}
	return false
}

func contentBlocksContainChildSpawnTool(value any) bool {
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			if contentBlocksContainChildSpawnTool(item) {
				return true
			}
		}
	case map[string]any:
		if strings.EqualFold(strings.TrimSpace(stringValue(current["type"])), "tool_use") &&
			isChildSpawnToolName(stringValue(current["name"])) {
			return true
		}
	}
	return false
}

func agentChildRequestSegments(req anthropicRequest) []string {
	for index := len(req.Messages) - 1; index >= 0; index-- {
		if strings.EqualFold(strings.TrimSpace(req.Messages[index].Role), "user") {
			return visibleAgentChildSegments(req.Messages[index].Content)
		}
	}
	return nil
}

func visibleAgentChildSegments(value any) []string {
	parts := make([]string, 0, 1)
	appendAgentChildVisibleText(&parts, value)
	return parts
}

func appendAgentChildVisibleText(parts *[]string, value any) {
	switch current := value.(type) {
	case string:
		if normalized := normalizedAgentChildText(current); normalized != "" {
			*parts = append(*parts, normalized)
		}
	case []any:
		for _, item := range current {
			appendAgentChildVisibleText(parts, item)
		}
	case map[string]any:
		appendAgentChildMapVisibleText(parts, current)
	}
}

func appendAgentChildMapVisibleText(parts *[]string, value map[string]any) {
	blockType := strings.ToLower(strings.TrimSpace(stringValue(value["type"])))
	switch blockType {
	case "text", "input_text":
		appendAgentChildVisibleText(parts, value["text"])
		return
	case "tool_result":
		appendAgentChildVisibleText(parts, value["content"])
		return
	}
	if content, ok := value["content"]; ok {
		appendAgentChildVisibleText(parts, content)
		return
	}
	if text, ok := value["text"]; ok {
		appendAgentChildVisibleText(parts, text)
	}
}

func agentChildRequestHasSubagentSystemMarker(req anthropicRequest) bool {
	for _, segment := range agentChildSystemSegments(req) {
		if isAgentChildSystemEnvelope(segment) {
			return true
		}
	}
	return false
}

func isAgentChildSystemEnvelope(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{
		// Claude Code's SDK child requests carry this billing header in the
		// Anthropic system blocks. It is the native protocol-level child
		// identity and remains present even when the human-readable envelope
		// text changes between Claude Code releases.
		"cc_is_subagent=true",
		"claude code subagent execution envelope",
		"you are a subagent continuing",
		"you are a subagent spawned",
		"you are a sub-agent continuing",
		"you are a sub-agent spawned",
	} {
		if strings.Contains(value, prefix) {
			return true
		}
	}
	return false
}

func agentChildSystemSegments(req anthropicRequest) []string {
	parts := make([]string, 0, 1)
	appendAgentChildVisibleText(&parts, req.System)
	return parts
}

func agentChildContinuationIdentity(req anthropicRequest, descriptor agentChildDescriptor) string {
	marker := agentChildStableSystemMarker(req)
	if marker == "" {
		return ""
	}
	childPrompt := normalizedAgentChildText(descriptor.prompt)
	if childPrompt == "" {
		childPrompt = normalizedAgentChildText(descriptor.description)
	}
	if childPrompt != "" && !agentChildRequestContainsUserText(req, childPrompt) {
		return ""
	}
	// The native child marker is the stable protocol identity. Claude Code can
	// rewrite the surrounding system envelope between continuation requests;
	// hashing that mutable text would reject a valid child and risk a visible
	// fail-closed response. The descriptor is spawn-time data and distinguishes
	// sibling child reservations that share the same session and marker.
	identity := strings.Join([]string{
		"marker=" + marker,
		"model=" + normalizedAgentChildText(descriptor.model),
		"prompt=" + normalizedAgentChildText(descriptor.prompt),
		"description=" + normalizedAgentChildText(descriptor.description),
		"workflow=" + boolIdentity(descriptor.workflow),
	}, "\n")
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func agentChildStableSystemMarker(req anthropicRequest) string {
	for _, segment := range agentChildSystemSegments(req) {
		value := strings.ToLower(strings.TrimSpace(segment))
		switch {
		case strings.Contains(value, "cc_is_subagent=true"):
			return "cc_is_subagent"
		case strings.Contains(value, "claude code subagent execution envelope"):
			return "claude_code_subagent"
		case strings.Contains(value, "you are a subagent continuing"),
			strings.Contains(value, "you are a subagent spawned"),
			strings.Contains(value, "you are a sub-agent continuing"),
			strings.Contains(value, "you are a sub-agent spawned"):
			return "subagent_envelope"
		}
	}
	return ""
}

func boolIdentity(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func agentChildRequestContainsUserText(req anthropicRequest, want string) bool {
	want = normalizedAgentChildText(want)
	if want == "" {
		return false
	}
	for index := range req.Messages {
		if !strings.EqualFold(strings.TrimSpace(req.Messages[index].Role), "user") {
			continue
		}
		for _, segment := range visibleAgentChildSegments(req.Messages[index].Content) {
			if segment == want {
				return true
			}
		}
	}
	return false
}

func agentChildRequestHasChildEnvelope(req anthropicRequest) bool {
	// The system envelope is the stable boundary Claude Code adds to child
	// requests. User text is deliberately not an identity signal: a parent can
	// quote both "subagent" and a child's prompt while continuing its own turn.
	return agentChildRequestHasSubagentSystemMarker(req)
}

func normalizedAgentChildText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func agentChildDescriptorsFromValue(value any) []agentChildDescriptor {
	normalized, ok := normalizedAgentChildResponseValue(value)
	if !ok {
		return nil
	}
	content, ok := anthropicResponseContentBlocks(normalized)
	if !ok {
		return nil
	}
	return agentChildDescriptorsFromContentBlocks(content)
}

func normalizedAgentChildResponseValue(value any) (any, bool) {
	switch value.(type) {
	case []any, map[string]any:
		return value, true
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		var normalized any
		if err := json.Unmarshal(raw, &normalized); err != nil {
			return nil, false
		}
		switch normalized.(type) {
		case []any, map[string]any:
			return normalized, true
		default:
			return nil, false
		}
	}
}

func anthropicResponseContentBlocks(value any) (any, bool) {
	switch current := value.(type) {
	case []any:
		return current, true
	case map[string]any:
		if content, ok := current["content"]; ok {
			return content, true
		}
		if _, hasType := current["type"]; hasType {
			return current, true
		}
	}
	return nil, false
}

func agentChildDescriptorsFromContentBlocks(value any) []agentChildDescriptor {
	var items []any
	switch current := value.(type) {
	case []any:
		items = current
	case map[string]any:
		items = []any{current}
	default:
		return nil
	}
	descriptors := make([]agentChildDescriptor, 0, len(items))
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok || !strings.EqualFold(strings.TrimSpace(stringValue(block["type"])), "tool_use") ||
			!isChildSpawnToolName(stringValue(block["name"])) {
			continue
		}
		input, ok := block["input"].(map[string]any)
		if !ok {
			continue
		}
		descriptors = append(descriptors, agentChildDescriptorsFromToolInput(stringValue(block["name"]), input)...)
	}
	return descriptors
}

func agentChildDescriptorsFromOpenAITools(tools []openAIToolCall) []agentChildDescriptor {
	descriptors := make([]agentChildDescriptor, 0, len(tools))
	for _, tool := range tools {
		if !isChildSpawnToolName(tool.Function.Name) {
			continue
		}
		input, ok := openAIToolArgumentsForTool(tool.Function.Name, tool.Function.Arguments).(map[string]any)
		if !ok {
			continue
		}
		descriptors = append(descriptors, agentChildDescriptorsFromToolInput(tool.Function.Name, input)...)
	}
	return descriptors
}

func agentChildDescriptorsFromToolInput(toolName string, input map[string]any) []agentChildDescriptor {
	if strings.EqualFold(strings.TrimSpace(toolName), "workflow") {
		script, ok := workflowScriptFromToolInput(input)
		if !ok {
			return nil
		}
		return workflowAgentDescriptors(script)
	}
	normalized := agentinput.Normalize(input)
	descriptor := agentChildDescriptor{
		model:       strings.TrimSpace(stringValue(normalized["model"])),
		prompt:      strings.TrimSpace(stringValue(normalized["prompt"])),
		description: strings.TrimSpace(stringValue(normalized["description"])),
	}
	if descriptor.prompt == "" {
		return nil
	}
	return []agentChildDescriptor{descriptor}
}

func invalidChildToolInputMessage(toolName string, input any) string {
	if isAgentChildToolName(toolName) {
		return invalidAgentToolInputMessage(toolName, input)
	}
	if strings.EqualFold(strings.TrimSpace(toolName), "workflow") {
		if _, ok := workflowScriptFromToolInput(input); !ok {
			return "CCR provider compatibility error: external provider returned invalid Workflow tool input. The workflow was not started."
		}
	}
	return ""
}

func workflowScriptFromToolInput(input any) (string, bool) {
	fields, ok := input.(map[string]any)
	if !ok {
		return "", false
	}
	script, ok := fields["script"].(string)
	if !ok || strings.TrimSpace(script) == "" {
		return "", false
	}
	return script, true
}

func workflowAgentDescriptors(script string) []agentChildDescriptor {
	prompts, hasDynamicCall := workflowAgentPrompts(script)
	descriptors := make([]agentChildDescriptor, 0, len(prompts)+1)
	for _, prompt := range prompts {
		descriptors = append(descriptors, agentChildDescriptor{workflow: true, prompt: prompt})
	}
	if hasDynamicCall {
		// A dynamic or malformed agent(...) call has no prompt that can be
		// correlated before the child request arrives. Keep one workflow-wide
		// reservation so it still inherits the active alias safely. A workflow
		// with no agent(...) call is not a child-spawning workflow and must not
		// reserve a future unrelated request.
		descriptors = append(descriptors, agentChildDescriptor{workflow: true})
	}
	return descriptors
}
