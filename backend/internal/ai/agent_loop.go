package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

type agentLoopResult struct {
	messages            []modelMessage
	parsed              analysisResponse
	finalContent        string
	finalProviderItems  []json.RawMessage
	finalDraftObserved  bool
	finalDraftAttempt   int
	draftPhase          string
	gcsFloorOnlyRetries int
}

// runAgenticLoop executes the bounded model and tool investigation and returns one parseable draft.
func (c *Client) runAgenticLoop(ctx context.Context, state *agentState, messages []modelMessage, schemas []tools.Schema) (agentLoopResult, error) {
	var finalContent string
	var finalProviderItems []json.RawMessage
	// The raw GCS byte floor gets at most one retry after all other floors pass.
	gcsFloorOnlyRetries := 0

	// Per-floor anti-thrash: track the calls + gcsBytes counters at the
	// time we last nudged so we can detect whether the model has made
	// progress on the unmet axis since then. A model that keeps coming
	// back tools-free without progressing gets accepted but not cached
	// so the loop doesn't burn iterations on a refusing model. Sentinel
	// -1 ensures the very first iteration's zero-state counts as progress.
	nudgedAtCalls := -1
	nudgedAtGCSBytes := -1

	// Available evidence from the initial ranked plan keeps the loop open, with
	// the same anti-thrash discipline as the floor nudge. Sentinel -1 makes the
	// first tools-free answer eligible even before any evidence was recorded.
	evidence := evidenceGate{nudgedAtRevision: -1}

	maxIters := state.opts.MaxIters

	// Fixed schema cost added to every size estimate so compaction budgets
	// against the real request, not just message content.
	schemaBytes := schemaPayloadBytes(schemas)
	headroom := contextHeadroomFor(state.opts)

	finalDraftObserved := false
	finalDraftAttempt := 0
	draftPhase := "initial"

	// When single_tool_call is on, request parallel_tool_calls=false so
	// compliant endpoints emit a single call. The client-side cap below still
	// trims endpoints that ignore the flag.
	var parallelToolCalls *bool
	if state.opts.SingleToolCall {
		f := false
		parallelToolCalls = &f
	}

agentLoop:
	for iter := 0; iter < maxIters; iter++ {
		var fits bool
		messages, fits = prepareContextRequest(ctx, messages, schemaBytes, headroom, "applied")
		if !fits {
			if fallback := state.promoteFallbackDraft(); fallback != nil {
				finalContent = fallback.content
				finalProviderItems = fallback.providerItems
				finalDraftObserved = true
				finalDraftAttempt = fallback.attempt
				recordTrace(ctx, TraceEvent{Kind: "context_headroom", Outcome: "best_draft", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
				log.Printf("  ⚠ agentic context headroom exhausted; publishing the best prior draft without another provider request")
				break agentLoop
			}
			recordTrace(ctx, TraceEvent{Kind: "context_headroom", Outcome: "unavailable", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
			return agentLoopResult{}, ErrContextHeadroom
		}
		// Reserve the final tools-enabled request for unread planned evidence.
		if iter+1 == maxIters && state.calls > 0 {
			coverage := state.planCoverage()
			if coverage.Applicable > 0 {
				outcome, unread := evidence.decide(state, coverage.UnmetGroups)
				if outcome == evidenceGateNudge {
					nudgeMessages := slices.Clone(messages)
					nudgeMessages = append(nudgeMessages, modelMessage{Role: "user", Content: strPtr(formatEvidenceHeadroomNudge(unread))})
					if prepared, nudgeFits := prepareContextRequest(ctx, nudgeMessages, schemaBytes, headroom, "evidence_nudge"); nudgeFits {
						messages = prepared
						evidence.recordNudge(state)
						draftPhase = "evidence_retry"
						log.Printf("  ↻ agentic evidence nudge: reserving the final tools-enabled iteration for %d unread evidence group(s)", len(unread))
					} else {
						outcome = evidenceGateTimeHeadroom
					}
				}
				recordEvidencePlanTrace(ctx, "iteration_headroom", outcome, coverage, unread, 0)
			}
		}
		requestStart := time.Now()
		resp, err := c.callModelRequest(ctx, modelRequest{
			Model: c.model, Messages: messages, Tools: schemas,
			ParallelToolCalls: parallelToolCalls, PromptCacheKey: state.promptCacheKey,
		})
		state.recentModelRequest = time.Since(requestStart)
		if err != nil {
			// Detect "tools not supported" on the first call only.
			if iter == 0 && isToolsUnsupportedError(err) {
				return agentLoopResult{}, fmt.Errorf("%w: %v", ErrToolsUnsupported, err)
			}
			// A retained parseable draft is better output than losing the whole
			// analysis to one failed follow-up request.
			if fallback := state.promoteFallbackDraft(); fallback != nil {
				finalContent = fallback.content
				finalProviderItems = fallback.providerItems
				finalDraftObserved = true
				finalDraftAttempt = fallback.attempt
				recordTrace(ctx, TraceEvent{Kind: "draft_recovery", Outcome: "model_request_error", SelectedAttempt: fallback.attempt})
				log.Printf("  ⚠ agentic request failed (%v); publishing the best prior draft", err)
				break agentLoop
			}
			return agentLoopResult{}, fmt.Errorf("agentic iter %d: %w", iter+1, err)
		}
		if !resp.HasMessage {
			return agentLoopResult{}, fmt.Errorf("agentic iter %d: empty choices", iter+1)
		}
		msg := resp.Message

		if len(msg.ToolCalls) > 0 && msg.Content != nil {
			if parsedCandidate, parsedOK := tryParseAnalysis(*msg.Content); parsedOK {
				candidateCritique := critiqueDraftWithContent(parsedCandidate, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, parsedCandidate), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
				if len(candidateCritique.MissingSkillEvidence) > 0 {
					if treeSet := state.artifactTreeSet(); treeSet != nil {
						pruneAbsentSkillEvidence(parsedCandidate, &candidateCritique, treeSet)
					}
				}
				candidateDraft := state.newDraftCandidate(draftPhase, *msg.Content, msg.ProviderItems, parsedCandidate, candidateCritique)
				semanticAccepted := draftPhase == "semantic_retry" && state.judgeObjected
				state.considerFallbackDraft(candidateDraft, semanticAccepted)
				recordTrace(ctx, TraceEvent{Kind: "draft_recovery", Outcome: "tool_bearing_candidate", SelectedAttempt: candidateDraft.attempt})
			}
		}

		if len(msg.ToolCalls) == 0 {
			candidate := ""
			if msg.Content != nil {
				candidate = *msg.Content
			}

			// Enforce per-project floors by nudging the model to
			// investigate further before accepting its final answer.
			// Skip the nudge when no floor is unmet, budgets are exhausted, or the
			// model has not progressed on any unmet floor since the last nudge.
			// Avoid fighting the tool-side "finalize now" signal. The per-axis progress check
			// covers the pathological list-only loop: a model calling
			// list_artifacts repeatedly raises calls but never gcsBytes
			// and would otherwise be re-nudged every iteration.
			parsedCandidate, parsedOK := tryParseAnalysis(candidate)
			var candidateCritique critiqueOutcome
			var candidateDraft *critiqueDraftCandidate
			if parsedOK {
				candidateCritique = critiqueDraftWithContent(parsedCandidate, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, parsedCandidate), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
				if len(candidateCritique.MissingSkillEvidence) > 0 {
					if treeSet := state.artifactTreeSet(); treeSet != nil {
						if n := pruneAbsentSkillEvidence(parsedCandidate, &candidateCritique, treeSet); n > 0 {
							log.Printf("  ⓘ skill-evidence: %d required group(s) absent from this build's artifacts; not held against the draft", n)
						}
					}
				}
				candidateDraft = state.newDraftCandidate(draftPhase, candidate, msg.ProviderItems, parsedCandidate, candidateCritique)
				semanticAccepted := draftPhase == "semantic_retry" && state.judgeObjected
				state.considerFallbackDraft(candidateDraft, semanticAccepted)
			}

			floors := evalFloors(state, state.opts)
			gcsFloorOnly := floors.gcsUnmet && !floors.callsUnmet
			if markGCSFloorRetryExhausted(ctx, state, state.opts, gcsFloorOnlyRetries) {
				floors = evalFloors(state, state.opts)
			}
			if floors.anyUnmet() && !state.budgetExhausted {
				progressed := false
				if floors.callsUnmet && state.calls > nudgedAtCalls {
					progressed = true
				}
				if floors.gcsUnmet && state.gcsBytes > nudgedAtGCSBytes {
					progressed = true
				}
				if progressed {
					echo := modelMessage{Role: "assistant", ProviderItems: msg.ProviderItems}
					if msg.Content != nil {
						echo.Content = msg.Content
					}
					messages = append(messages, echo, modelMessage{
						Role:    "user",
						Content: strPtr(formatFloorsNudge(state, state.opts)),
					})
					var fits bool
					messages, fits = prepareContextRequest(ctx, messages, schemaBytes, headroom, "floor_nudge")
					if !fits {
						if fallback := state.promoteFallbackDraft(); fallback != nil {
							finalContent = fallback.content
							finalProviderItems = fallback.providerItems
							finalDraftAttempt = fallback.attempt
						} else {
							finalContent = candidate
							finalProviderItems = msg.ProviderItems
							if candidateDraft != nil {
								finalDraftAttempt = candidateDraft.attempt
							}
						}
						finalDraftObserved = parsedOK
						recordTrace(ctx, TraceEvent{Kind: "context_headroom", Outcome: "retry_denied", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
						recordTrace(ctx, TraceEvent{Kind: "context_headroom", Outcome: "best_draft", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
						break agentLoop
					}
					if gcsFloorOnly {
						// Leave room for the tools-enabled response when the nudge lands
						// on the configured iteration boundary.
						if iter+1 >= maxIters {
							maxIters++
						}
						gcsFloorOnlyRetries++
					}
					nudgedAtCalls = state.calls
					nudgedAtGCSBytes = state.gcsBytes
					draftPhase = "floor_retry"
					recordTrace(ctx, TraceEvent{Kind: "floor_nudge", Outcome: "retry", Status: floors.traceStatus(), ToolCallCount: state.calls, Bytes: state.gcsBytes})
					log.Printf("  ↻ agentic nudge: tool_calls=%d/min=%d, gcs_kb=%d/min=%d, asking model to investigate further",
						state.calls, state.opts.MinToolCalls, state.gcsBytes/1024, state.opts.MinGCSBytes/1024)
					continue
				}
			}

			// Evidence gate. The initial ranked plan already resolved candidate
			// paths for every applicable group; a group with candidates that the
			// model never read is available evidence left unread, so reopen the
			// investigation instead of finalizing on a partial picture. Groups
			// the draft's own prose newly required are included, since the plan
			// never had the chance to show them. Floors measure effort; this
			// measures whether the available evidence was actually consulted.
			coverage := state.planCoverage()
			unreadGroups := coverage.UnmetGroups
			draftTriggered := 0
			if parsedOK {
				extra := state.draftTriggeredEvidenceGroups(candidateCritique, unreadGroups)
				draftTriggered = len(extra)
				unreadGroups = append(unreadGroups, extra...)
			}
			if coverage.Applicable > 0 || len(unreadGroups) > 0 {
				outcome, unread := evidence.decide(state, unreadGroups)
				recordEvidencePlanTrace(ctx, "voluntary_finalize", outcome, coverage, unread, draftTriggered)
				if outcome == evidenceGateNudge {
					// Seed the selected draft before reopening. Otherwise a later,
					// worse draft becomes the first candidate the critique gate
					// sees and wins against an empty selection.
					if candidateDraft != nil {
						state.considerDraft(candidateDraft, draftPhase == "semantic_retry" && state.judgeObjected)
					}
					echo := modelMessage{Role: "assistant", ProviderItems: msg.ProviderItems}
					if msg.Content != nil {
						echo.Content = msg.Content
					}
					messages = append(messages, echo, modelMessage{
						Role:    "user",
						Content: strPtr(formatEvidenceNudge(unread)),
					})
					var fits bool
					messages, fits = prepareContextRequest(ctx, messages, schemaBytes, headroom, "evidence_nudge")
					if !fits {
						if fallback := state.promoteFallbackDraft(); fallback != nil {
							finalContent = fallback.content
							finalProviderItems = fallback.providerItems
							finalDraftAttempt = fallback.attempt
						} else {
							finalContent = candidate
							finalProviderItems = msg.ProviderItems
							if candidateDraft != nil {
								finalDraftAttempt = candidateDraft.attempt
							}
						}
						finalDraftObserved = parsedOK
						recordTrace(ctx, TraceEvent{Kind: "context_headroom", Outcome: "retry_denied", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
						recordTrace(ctx, TraceEvent{Kind: "context_headroom", Outcome: "best_draft", ContextLimitTokens: headroom.limitTokens, ReservedTokens: headroom.reservedTokens})
						break agentLoop
					}
					// Leave room for the tools-enabled response when the nudge lands
					// on the configured iteration boundary.
					if iter+1 >= maxIters {
						maxIters++
					}
					evidence.recordNudge(state)
					draftPhase = "evidence_retry"
					log.Printf("  ↻ agentic evidence nudge: %d evidence group(s) still unread, asking model to read them before finalizing", len(unread))
					continue
				}
			}

			// Critique gate (always on). Re-prompts the model with targeted
			// feedback when the draft punts, hallucinates, fabricates an import
			// path, or fails recipe-driven evidence. Only fires on parseable
			// candidates; unparseable finals fall through to runFinalizeRound
			// below.
			if parsed, ok := parsedCandidate, parsedOK; ok {
				out := candidateCritique
				semanticAccepted := draftPhase == "semantic_retry" && state.judgeObjected
				state.considerDraft(candidateDraft, semanticAccepted)
				if out.Passed {
					recordTrace(ctx, critiqueTraceEvent("passed", out))
					// The initial semantic review runs only on the selected draft.
					// An objected draft may spend one tools-free refinalization and
					// one revision-review call. Failures preserve the selected draft.
					if state.opts.SemanticJudge.enabled() && !state.judgeRan && state.bestDraft == candidateDraft {
						state.judgeRan = true
						result, err := c.semanticCritiqueTracked(ctx, state, candidateDraft.attempt, semanticJudgeStageDraft, parsed, nil, nil, headroom)
						switch {
						case err != nil:
							recordTrace(ctx, semanticJudgeTraceEvent(semanticJudgeStageDraft, "error", result, "semantic_judge_error"))
							log.Printf("  ⓘ semantic judge: skipped (%v)", err)
						case len(result.Findings) > 0:
							recordTrace(ctx, semanticJudgeTraceEvent(semanticJudgeStageDraft, "objected", result, ""))
							state.judgeObjected = true
							state.semanticFindings = semanticFindingClassList(result.Findings)
							prior := parsed
							echo := modelMessage{Role: "assistant", ProviderItems: msg.ProviderItems}
							if msg.Content != nil {
								echo.Content = msg.Content
							}
							repairMessages := append(messages, echo, modelMessage{
								Role:    "user",
								Content: strPtr(formatSemanticFindings(result.Findings)),
							})
							revised, revisedItems, safe := c.runFinalizeRoundTracked(ctx, state, repairMessages, headroom)
							if safe {
								if rp, ok := tryParseAnalysis(revised); ok {
									revisedCritique := critiqueDraftWithContent(rp, state.readArtifactsFull, state.readArtifactsBase, state.evidenceContentByPath, state.readSourceFull, matchSkillsForDraft(state, rp), state.consecutiveFailures, analysisCitationContext{Evidence: state.analysisEvidence, Full: state.analysisEvidenceFull})
									if len(revisedCritique.MissingSkillEvidence) > 0 {
										if treeSet := state.artifactTreeSet(); treeSet != nil {
											pruneAbsentSkillEvidence(rp, &revisedCritique, treeSet)
										}
									}
									semanticCandidate := state.newDraftCandidate("semantic_retry", revised, revisedItems, rp, revisedCritique)
									policy := effectiveCritiqueCachePolicy(state.opts.CritiqueCachePolicy)
									decision := c.reviewSemanticRevision(ctx, state, prior, result.Findings, semanticCandidate, headroom, policy)
									if !decision.accepted && semanticRevisionRejected(decision, semanticCandidate, policy) {
										state.judgeRevisionRejected = true
										recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revision_rejected", IssueCount: len(semanticCandidate.semanticFindingClasses)})
									} else if decision.accepted {
										state.considerFallbackDraftForPolicy(semanticCandidate, true, policy)
										state.judgeRevised = true
										state.semanticFindings = nil
										recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revised"})
									} else {
										recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revision_not_selected"})
									}
								} else {
									state.judgeRevisionRejected = true
									recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revision_unparseable", IssueCount: len(result.Findings)})
								}
							} else {
								state.judgeRevisionRejected = true
								recordTrace(ctx, TraceEvent{Kind: "semantic_judge", Status: semanticJudgeStageRevision, Outcome: "revision_denied", IssueCount: len(result.Findings)})
							}
							fallback := state.promoteFallbackDraft()
							finalContent = fallback.content
							finalProviderItems = fallback.providerItems
							finalDraftObserved = true
							finalDraftAttempt = fallback.attempt
							state.critiquePassed = fallback.quality.Passed
							break agentLoop
						default:
							recordTrace(ctx, semanticJudgeTraceEvent(semanticJudgeStageDraft, "passed", result, ""))
							log.Printf("  ✓ semantic judge: no findings")
							state.considerDraft(candidateDraft, true)
							state.considerFallbackDraft(candidateDraft, true)
						}
					}
					// Reaching acceptance after the judge objected on an earlier
					// draft means its objections drove an accepted revision.
					if state.judgeObjected && !state.judgeRevisionRejected && state.bestDraft != nil && candidateDraft != nil && state.bestDraft.attempt == candidateDraft.attempt {
						state.judgeRevised = true
						state.semanticFindings = nil
					}
					state.critiquePassed = state.bestDraft != nil && state.bestDraft.quality.Passed
				} else {
					state.critiquePassed = false
				}
			}

			if parsedOK && state.bestDraft != nil {
				finalContent = state.bestDraft.content
				finalProviderItems = state.bestDraft.providerItems
				finalDraftAttempt = state.bestDraft.attempt
			} else {
				finalContent = candidate
				finalProviderItems = msg.ProviderItems
				if candidateDraft != nil {
					finalDraftAttempt = candidateDraft.attempt
				}
			}
			finalDraftObserved = parsedOK
			break
		}

		toolCalls, dropped := limitToolCalls(msg.ToolCalls, state.opts.SingleToolCall)
		if dropped > 0 {
			log.Printf("  ⤵ single_tool_call: model returned %d tool calls; executing the first and dropping %d (model may re-request them)",
				len(msg.ToolCalls), dropped)
		}

		echoCalls, skippedOutputs := continuationCalls(c.apiMode, msg, toolCalls)
		echo := modelMessage{Role: "assistant", ToolCalls: echoCalls, ProviderItems: msg.ProviderItems}
		if msg.Content != nil {
			echo.Content = msg.Content
		}
		messages = append(messages, echo)

		messages = append(messages, skippedOutputs...)

		for _, tc := range toolCalls {
			result := dispatchAgenticTool(ctx, state, tc)
			state.modelBytes += len(result)
			messages = append(messages, modelMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    strPtr(result),
			})
		}
	}

	// If the model never returned a tools-free final message, OR returned one
	// without parseable JSON, force a finalize round with tools omitted.
	parsed, ok := tryParseAnalysis(finalContent)
	if !ok {
		coverage := state.planCoverage()
		if coverage.Applicable > 0 {
			outcome, unread := evidence.decide(state, coverage.UnmetGroups)
			if outcome == evidenceGateNudge {
				outcome = evidenceGateIterationExhausted
			}
			recordEvidencePlanTrace(ctx, "forced_finalize", outcome, coverage, unread, 0)
		}
		var safe bool
		finalContent, finalProviderItems, safe = c.runFinalizeRoundTracked(ctx, state, messages, headroom)
		if !safe {
			return agentLoopResult{}, ErrContextHeadroom
		}
		parsed, ok = tryParseAnalysis(finalContent)
		if ok {
			recordTrace(ctx, TraceEvent{Kind: "finalize_parse", Outcome: "accepted"})
		} else {
			code := "invalid_structured_response"
			if strings.TrimSpace(finalContent) == "" {
				code = "empty_content"
			}
			recordTrace(ctx, TraceEvent{Kind: "finalize_parse", Outcome: "rejected", ErrorCode: code})
		}
	}
	if !ok && state.fallbackDraft != nil {
		fallback := state.promoteFallbackDraft()
		finalContent = fallback.content
		finalProviderItems = fallback.providerItems
		finalDraftObserved = true
		finalDraftAttempt = fallback.attempt
		parsed, ok = tryParseAnalysis(finalContent)
		recordTrace(ctx, TraceEvent{Kind: "finalize_recovery", Outcome: "retained_draft", SelectedAttempt: fallback.attempt})
		log.Printf("  ⚠ agentic repair: finalize did not parse; keeping selected draft")
	}
	if !ok {
		recordTrace(ctx, TraceEvent{Kind: "finalize_recovery", Outcome: "rejected", ErrorCode: "invalid_structured_response"})
		return agentLoopResult{}, ErrRejectedAnalysis
	}

	return agentLoopResult{
		messages: messages, parsed: parsed,
		finalContent: finalContent, finalProviderItems: finalProviderItems,
		finalDraftObserved: finalDraftObserved, finalDraftAttempt: finalDraftAttempt, draftPhase: draftPhase,
		gcsFloorOnlyRetries: gcsFloorOnlyRetries,
	}, nil
}
