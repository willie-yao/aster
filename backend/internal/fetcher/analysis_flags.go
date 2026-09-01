package fetcher

import (
	"flag"
)

// BindAnalysisFlags adds the analysis flags used by fetcher and worker.
func BindAnalysisFlags(fs *flag.FlagSet, opts *Options) {
	fs.IntVar(&opts.AIMaxOutputTokens, "ai-max-output-tokens", 0, "optional authoritative analysis output-token cap; zero uses the provider default")

}
