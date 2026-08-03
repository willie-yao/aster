package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
)

const (
	defaultStructuredResponseBytes int64 = 128 << 10
	maxStructuredCandidates              = 32
	maxStructuredCandidateStarts         = 4096
)

// ResponseFormat describes one strict JSON Schema response.
type ResponseFormat struct {
	Name        string
	Description string
	Schema      map[string]any
}

// ToolChoice forces one named function call.
type ToolChoice struct {
	Name string
}

// StructuredValidator accepts one complete JSON object. It must decode and
// validate the expected fields before returning nil.
type StructuredValidator func(json.RawMessage) error

// CompleteStructured requests a schema-bound object and accepts it only after
// deterministic caller validation. Provider schema support is preferred, then
// a forced function call, then bounded extraction from a plain completion.
func (c *Client) CompleteStructured(ctx context.Context, system, user string, format ResponseFormat, validate StructuredValidator) error {
	if validate == nil {
		return fmt.Errorf("structured completion validator is required")
	}
	if strings.TrimSpace(format.Name) == "" || len(format.Schema) == 0 {
		return fmt.Errorf("structured completion schema is required")
	}
	messages := []modelMessage{
		{Role: "system", Content: strPtr(system)},
		{Role: "user", Content: strPtr(user)},
	}
	parallel := false
	attempts := []modelRequest{
		{
			Model: c.model, Messages: messages, ResponseFormat: &format,
			MaxResponseBytes: defaultStructuredResponseBytes, OmitReasoning: true,
		},
		{
			Model: c.model, Messages: messages,
			Tools: []tools.Schema{{
				Type: "function",
				Function: tools.FunctionDecl{
					Name: format.Name, Description: format.Description,
					Parameters: format.Schema, Strict: true,
				},
			}},
			ToolChoice: &ToolChoice{Name: format.Name}, ParallelToolCalls: &parallel,
			MaxResponseBytes: defaultStructuredResponseBytes, OmitReasoning: true,
		},
		{
			Model: c.model, Messages: messages,
			MaxResponseBytes: defaultStructuredResponseBytes, OmitReasoning: true,
		},
	}

	for index, request := range attempts {
		resp, err := c.callModelRequest(ctx, request)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if index < len(attempts)-1 && structuredFallbackAllowed(err) {
				continue
			}
			return structuredFailure("provider request failed")
		}
		var raw string
		if request.ToolChoice != nil {
			if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != format.Name {
				continue
			}
			raw = resp.Message.ToolCalls[0].Function.Arguments
		} else if resp.HasMessage && resp.Message.Content != nil {
			raw = *resp.Message.Content
		} else {
			continue
		}
		if err := validateStructuredCandidates(raw, validate); err != nil {
			continue
		}
		return nil
	}
	return structuredFailure("no valid structured response")
}

func structuredFallbackAllowed(err error) bool {
	var httpErr *modelHTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	switch httpErr.StatusCode {
	case 400, 404, 405, 415, 422:
		return true
	default:
		return false
	}
}

func structuredFailure(reason string) error {
	return fmt.Errorf("structured completion rejected: %s", reason)
}

func validateStructuredCandidates(raw string, validate StructuredValidator) error {
	if len(raw) > int(defaultStructuredResponseBytes) {
		return fmt.Errorf("structured response exceeds %d bytes", defaultStructuredResponseBytes)
	}
	trimmed := strings.TrimSpace(raw)
	if json.Valid([]byte(trimmed)) {
		if err := validate(json.RawMessage(trimmed)); err == nil {
			return nil
		}
	}
	return validateExtractedCandidates(raw, validate)
}

func validateExtractedCandidates(raw string, validate StructuredValidator) error {
	data := []byte(raw)
	type acceptedCandidate struct {
		canonical []byte
	}
	var accepted []acceptedCandidate
	seenStarts := 0
	decodedCandidates := 0
	for index, b := range data {
		if b != '{' {
			continue
		}
		seenStarts++
		if seenStarts > maxStructuredCandidateStarts {
			return fmt.Errorf("structured response contains too many candidate starts")
		}
		decoder := json.NewDecoder(bytes.NewReader(data[index:]))
		decoder.UseNumber()
		var candidate json.RawMessage
		if err := decoder.Decode(&candidate); err != nil || len(candidate) == 0 {
			continue
		}
		decodedCandidates++
		if decodedCandidates > maxStructuredCandidates {
			return fmt.Errorf("structured response contains too many JSON candidates")
		}
		if err := validate(candidate); err != nil {
			continue
		}
		canonical, err := canonicalJSON(candidate)
		if err != nil {
			continue
		}
		accepted = append(accepted, acceptedCandidate{canonical: canonical})
	}
	if len(accepted) == 0 {
		return fmt.Errorf("structured response contains no valid JSON object")
	}
	last := accepted[len(accepted)-1].canonical
	for _, candidate := range accepted[:len(accepted)-1] {
		if !bytes.Equal(candidate.canonical, last) {
			return fmt.Errorf("structured response contains conflicting valid JSON objects")
		}
	}
	return nil
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
