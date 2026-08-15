package ai

import "context"

// EvidenceReadSource identifies which analysis path initiated an artifact read.
type EvidenceReadSource string

const (
	EvidenceReadSourceModelTool       EvidenceReadSource = "model_tool"
	EvidenceReadSourceRepairInjection EvidenceReadSource = "repair_injection"
)

type evidenceReadSourceContextKey struct{}

func withEvidenceReadSource(ctx context.Context, source EvidenceReadSource) context.Context {
	return context.WithValue(ctx, evidenceReadSourceContextKey{}, source)
}

// EvidenceReadSourceFromContext returns the content-free artifact-read source.
func EvidenceReadSourceFromContext(ctx context.Context) EvidenceReadSource {
	if ctx == nil {
		return ""
	}
	source, _ := ctx.Value(evidenceReadSourceContextKey{}).(EvidenceReadSource)
	return source
}
