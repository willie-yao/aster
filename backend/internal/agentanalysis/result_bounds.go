package agentanalysis

const (
	maxResultBytes        = 128 << 10
	maxSummaryBytes       = 2 << 10
	maxRootCauseBytes     = 24 << 10
	maxSuggestedFixBytes  = 12 << 10
	maxUnresolvedBytes    = 4 << 10
	maxRelevantFiles      = 20
	maxEvidenceCitations  = 20
	maxSourceCitations    = 10
	maxUnresolvedDetails  = 20
	maxCitationQuoteBytes = 2 << 10
	maxCitationLines      = 200
	maxAgentPromptBytes   = 112 << 10
)
