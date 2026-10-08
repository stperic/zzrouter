package protocol

import "encoding/json"

type imageBlock struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

// HasImages inspects image-bearing inference fields, without searching tool
// schemas, metadata, or textual mentions of images. Malformed requests remain
// the protocol validator's responsibility.
func HasImages(body []byte) bool {
	var req struct {
		Messages []struct {
			Content json.RawMessage   `json:"content"`
			Images  []json.RawMessage `json:"images"`
		} `json:"messages"`
		Input  json.RawMessage   `json:"input"`
		Images []json.RawMessage `json:"images"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	if len(req.Images) > 0 {
		return true
	}
	for _, m := range req.Messages {
		if len(m.Images) > 0 || contentHasImages(m.Content) {
			return true
		}
	}
	var input []imageBlock
	if json.Unmarshal(req.Input, &input) == nil {
		for _, item := range input {
			if item.Type == "input_image" || ((item.Type == "message" || item.Type == "") && contentHasImages(item.Content)) {
				return true
			}
		}
	}
	return false
}

func contentHasImages(raw json.RawMessage) bool {
	var blocks []imageBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if isImageBlock(b.Type) {
			return true
		}
		if b.Type == "tool_result" {
			var result []imageBlock
			if json.Unmarshal(b.Content, &result) == nil {
				for _, part := range result {
					if isImageBlock(part.Type) {
						return true
					}
				}
			}
		}
	}
	return false
}

func isImageBlock(kind string) bool {
	return kind == "image" || kind == "image_url" || kind == "input_image"
}
