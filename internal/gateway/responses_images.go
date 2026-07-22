package gateway

import (
	"context"
	"fmt"
	"strings"
)

func (h *handler) normalizeResponsesImages(ctx context.Context, req anthropicRequest) (anthropicRequest, error) {
	resolver := newImageSourceResolver(h.cfg.ImageHTTPClient, newImageFetchBudget(int64(maxURLImageBytes)))
	var err error
	req.System, err = normalizeResponsesContent(ctx, req.System, resolver)
	if err != nil {
		return anthropicRequest{}, fmt.Errorf("normalizing system image input: %w", err)
	}
	for index := range req.Messages {
		req.Messages[index].Content, err = normalizeResponsesContent(ctx, req.Messages[index].Content, resolver)
		if err != nil {
			return anthropicRequest{}, fmt.Errorf("normalizing message %d image input: %w", index, err)
		}
	}
	return req, nil
}

func normalizeResponsesContent(ctx context.Context, value any, resolver imageSourceResolver) (any, error) {
	switch content := value.(type) {
	case nil, string:
		return content, nil
	case []any:
		return normalizeResponsesContentList(ctx, content, resolver)
	case map[string]any:
		return normalizeResponsesContentObject(ctx, content, resolver)
	default:
		return nil, fmt.Errorf("image content must be a string, array, or object, got %T", value)
	}
}

func normalizeResponsesContentList(ctx context.Context, content []any, resolver imageSourceResolver) ([]any, error) {
	cloned := make([]any, len(content))
	for index := range content {
		converted, err := normalizeResponsesContent(ctx, content[index], resolver)
		if err != nil {
			return nil, err
		}
		cloned[index] = converted
	}
	return cloned, nil
}

func normalizeResponsesContentObject(ctx context.Context, content map[string]any, resolver imageSourceResolver) (map[string]any, error) {
	cloned := cloneResponsesContentObject(content)
	switch strings.ToLower(strings.TrimSpace(stringValue(cloned["type"]))) {
	case "image":
		return normalizeResponsesImageBlock(ctx, cloned, resolver)
	case "tool_result":
		converted, err := normalizeResponsesContent(ctx, cloned["content"], resolver)
		if err != nil {
			return nil, err
		}
		cloned["content"] = converted
	}
	return cloned, nil
}

func cloneResponsesContentObject(content map[string]any) map[string]any {
	cloned := make(map[string]any, len(content))
	for key, item := range content {
		cloned[key] = item
	}
	return cloned
}

func normalizeResponsesImageBlock(ctx context.Context, block map[string]any, resolver imageSourceResolver) (map[string]any, error) {
	image, err := resolver(ctx, block)
	if err != nil {
		return nil, err
	}
	source, err := base64SourceFromImagePart(image)
	if err != nil {
		return nil, err
	}
	block["source"] = source
	return block, nil
}

func base64SourceFromImagePart(part map[string]any) (map[string]any, error) {
	imageURL, ok := part["image_url"].(map[string]string)
	if !ok {
		return nil, fmt.Errorf("image resolver returned an invalid image URL part")
	}
	value := imageURL["url"]
	const prefix = "data:"
	if !strings.HasPrefix(value, prefix) {
		return nil, fmt.Errorf("image resolver returned a non-data image URL")
	}
	mediaType, data, found := strings.Cut(strings.TrimPrefix(value, prefix), ";base64,")
	if !found || !supportedImageMediaType(mediaType) || data == "" {
		return nil, fmt.Errorf("image resolver returned an invalid data image URL")
	}
	return map[string]any{
		"type":       "base64",
		"media_type": normalizeImageMediaType(mediaType),
		"data":       data,
	}, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
