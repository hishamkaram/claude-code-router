// Package responses adapts Anthropic Messages JSON to the OpenAI Responses API.
//
// The package intentionally has no dependency on internal/gateway so provider
// integrations can use it without introducing import cycles. It is a wire
// adapter only: it converts already-supplied JSON and never fetches remote
// images, logs provider payloads, or stores credentials.
//
// RequestFromAnthropicMessagesJSON converts a raw Anthropic Messages request
// into a typed Responses request. It supports text, image blocks with base64 or
// HTTPS URL sources, text tool results, screenshot-like tool-result image
// blocks, ordinary function tools, and native Anthropic computer tools.
//
// AnthropicResponseFromResponsesJSON converts a non-stream Responses API
// response into an Anthropic-compatible message object with text and tool_use
// content blocks.
package responses
