package transport

import (
	"testing"
)

func TestStructuredWireMappings(t *testing.T) {
	format := ResponseFormat{Name: "return_body", Description: "Return a body.", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"body": map[string]any{"type": "string"},
		},
		"required": []string{"body"}, "additionalProperties": false,
	}}
	chatFormat := encodeChatResponseFormat(&format)
	if chatFormat == nil || chatFormat.Type != "json_schema" || !chatFormat.JSONSchema.Strict || chatFormat.JSONSchema.Name != format.Name {
		t.Fatalf("chat response format = %+v", chatFormat)
	}
	chatChoice := encodeChatToolChoice(&ToolChoice{Name: format.Name})
	if chatChoice == nil || chatChoice.Type != "function" || chatChoice.Function.Name != format.Name {
		t.Fatalf("chat tool choice = %+v", chatChoice)
	}
	responsesText := encodeResponsesText(&format)
	if responsesText == nil || responsesText.Format.Type != "json_schema" || !responsesText.Format.Strict || responsesText.Format.Name != format.Name {
		t.Fatalf("responses text format = %+v", responsesText)
	}
	responsesChoice := encodeResponsesToolChoice(&ToolChoice{Name: format.Name})
	if responsesChoice == nil || responsesChoice.Type != "function" || responsesChoice.Name != format.Name {
		t.Fatalf("responses tool choice = %+v", responsesChoice)
	}
}
