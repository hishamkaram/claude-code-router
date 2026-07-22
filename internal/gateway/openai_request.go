package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type openAIModelRoute struct {
	alias                         string
	providerName                  string
	providerModel                 string
	requestModel                  string
	suppressIdentitySystemMessage bool
	forceDisableParallelTools     bool
}

func (route openAIModelRoute) identityMessage() (openAIMessage, bool) {
	content, ok := route.identityContent()
	if !ok {
		return openAIMessage{}, false
	}
	return openAIMessage{Role: "system", Content: content}, true
}

func (route openAIModelRoute) identityContent() (string, bool) {
	if route.suppressIdentitySystemMessage {
		return "", false
	}
	alias := strings.TrimSpace(route.alias)
	providerName := strings.TrimSpace(route.providerName)
	providerModel := strings.TrimSpace(route.providerModel)
	requestModel := strings.TrimSpace(route.requestModel)
	if alias == "" || providerModel == "" {
		return "", false
	}
	content := fmt.Sprintf("CCR route context: this Claude Code request is using CCR alias %q routed to provider model %q.", alias, providerModel)
	if providerName != "" {
		content = fmt.Sprintf("CCR route context: this Claude Code request is using CCR alias %q on provider %q routed to provider model %q.", alias, providerName, providerModel)
	}
	if requestModel != "" && requestModel != alias {
		content += fmt.Sprintf(" Claude Code requested model ID %q.", requestModel)
	}
	content += " If asked which model is active, answer from this CCR route context and do not reuse model names from earlier assistant turns, because previous turns may have used a different route."
	return content, true
}

func toOpenAIChatRequest(req anthropicRequest, route openAIModelRoute) (openAIChatRequest, error) {
	return toOpenAIChatRequestWithResolver(context.Background(), req, route, newChatImageSourceResolver(nil))
}

func (h *handler) toOpenAIChatRequest(ctx context.Context, req anthropicRequest, route openAIModelRoute) (openAIChatRequest, error) {
	return toOpenAIChatRequestWithResolver(ctx, req, route, newChatImageSourceResolver(h.cfg.ImageHTTPClient))
}

func newChatImageSourceResolver(client *http.Client) imageSourceResolver {
	return newImageSourceResolver(client, newImageFetchBudget(int64(maxURLImageBytes)))
}

func toOpenAIChatRequestWithResolver(ctx context.Context, req anthropicRequest, route openAIModelRoute, resolver imageSourceResolver) (openAIChatRequest, error) {
	options, err := openAIOptionsFromAnthropic(req)
	if err != nil {
		return openAIChatRequest{}, err
	}
	tools, err := openAIToolsFromAnthropic(req.Tools)
	if err != nil {
		return openAIChatRequest{}, err
	}
	toolChoice, parallelTools, err := openAIToolChoiceFromAnthropic(req.ToolChoice)
	if err != nil {
		return openAIChatRequest{}, err
	}
	if route.forceDisableParallelTools && len(tools) > 0 {
		parallelDisabled := false
		parallelTools = &parallelDisabled
	}
	messages, err := openAIMessagesFromRequestWithResolver(ctx, req, route, resolver)
	if err != nil {
		return openAIChatRequest{}, err
	}
	return openAIChatRequest{
		Model:           route.providerModel,
		Messages:        messages,
		MaxTokens:       req.MaxTokens,
		Temperature:     req.Temperature,
		Stop:            req.StopSequences,
		Stream:          false,
		User:            options.user,
		ReasoningEffort: options.reasoningEffort,
		ResponseFormat:  options.responseFormat,
		Tools:           tools,
		ToolChoice:      toolChoice,
		ParallelTools:   parallelTools,
	}, nil
}

func openAIMessagesFromRequestWithResolver(ctx context.Context, req anthropicRequest, route openAIModelRoute, resolver imageSourceResolver) ([]openAIMessage, error) {
	identityMessage, includeIdentity := route.identityMessage()
	includeIdentity = includeIdentity && latestUserAsksModelIdentity(req.Messages)
	messages := make([]openAIMessage, 0, len(req.Messages)+2)
	if req.System != nil {
		text, err := anthropicContentText(req.System)
		if err != nil {
			return nil, fmt.Errorf("unsupported system content: %w", err)
		}
		if text != "" {
			messages = append(messages, openAIMessage{Role: "system", Content: text})
		}
	}
	identityAdded := false
	for _, message := range req.Messages {
		if includeIdentity && !identityAdded && message.Role != "system" {
			messages = append(messages, identityMessage)
			identityAdded = true
		}
		converted, err := openAIMessagesFromAnthropicWithResolver(ctx, message, resolver)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	if includeIdentity && !identityAdded {
		messages = append(messages, identityMessage)
	}
	return messages, nil
}

func latestUserAsksModelIdentity(messages []anthropicMessage) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		return asksModelIdentity(messageText(messages[i].Content))
	}
	return false
}

func messageText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			if blockType != "text" {
				continue
			}
			text, _ := block["text"].(string)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func asksModelIdentity(text string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(text), " "))
	if normalized == "" {
		return false
	}
	for _, phrase := range []string{
		"which model are you",
		"which model you are",
		"what model are you",
		"what model you are",
		"which model is active",
		"what model is active",
		"model are you",
		"model you are",
		"your model",
		"current model",
		"active model",
		"which llm are you",
		"what llm are you",
		"who are you",
	} {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
}
