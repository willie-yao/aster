package main

import (
	"log"
	"net/http"
)

// validate_analysis is the reference template for a self-registered quality
// tool. It deterministically checks that every artifact path an analysis cites
// exists in this build's tree (a 1-byte read via the Browser): the single
// high-value check kept from the engine's critique gate (hallucinated-citation
// guard), exposed Orka-natively as a tool a reviewer agent must call before
// approving an analysis.
//
// Pattern every quality tool follows:
//  1. one file named after the tool,
//  2. an init() that calls registerQTool with the exact /tool/<name> route,
//  3. a handler(*toolEnv, w, r) that reads args, does deterministic work over
//     env.browser (or env.backend for cross-build), and writes JSON.
func init() {
	registerQTool("/tool/validate_analysis", validateAnalysis)
}

func validateAnalysis(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args struct {
		Paths []string `json:"paths"`
	}
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := requestCtx(r)
	defer cancel()

	present, missing := []string{}, []string{}
	for _, p := range args.Paths {
		if p == "" {
			continue
		}
		if _, _, err := env.browser.Read(ctx, p, 0, 1); err != nil {
			missing = append(missing, p)
		} else {
			present = append(present, p)
		}
	}
	log.Printf("✔ validate_analysis paths=%d present=%d missing=%d", len(args.Paths), len(present), len(missing))
	writeJSON(w, map[string]any{
		"checked":     len(present) + len(missing),
		"present":     present,
		"missing":     missing,
		"all_present": len(missing) == 0,
	})
}
