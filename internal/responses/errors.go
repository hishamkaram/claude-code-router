package responses

import "errors"

var (
	// ErrUnsupportedPDF reports an Anthropic PDF/document block that this
	// adapter cannot safely translate to Responses input.
	ErrUnsupportedPDF = errors.New("responses adapter: unsupported PDF content")

	// ErrUnsupportedAudio reports an Anthropic audio block that this adapter
	// cannot safely translate to Responses input.
	ErrUnsupportedAudio = errors.New("responses adapter: unsupported audio content")

	// ErrNativeCUARequired reports a screenshot-style tool result without enough
	// native computer-use context to map it to computer_call_output.
	ErrNativeCUARequired = errors.New("responses adapter: native computer-use request required")

	// ErrMalformedProviderOutput reports a Responses payload that cannot be
	// represented as a safe Anthropic-compatible message.
	ErrMalformedProviderOutput = errors.New("responses adapter: malformed provider output")

	// ErrUnsuccessfulProviderStatus reports a Responses payload whose terminal
	// status cannot be represented as a successful Anthropic-compatible turn.
	ErrUnsuccessfulProviderStatus = errors.New("responses adapter: unsuccessful provider status")
)
