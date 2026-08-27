package devmock

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/analysischat"
)

// chatMaxTurns bounds a mock conversation, matching how the real service caps
// one session so the turn counter in the UI behaves.
const chatMaxTurns = 12

// chatSessionTTL bounds a mock session.
const chatSessionTTL = 24 * time.Hour

// Attempt outcomes the transcript understands. A succeeded attempt whose
// answer is in the transcript is folded into that message rather than shown on
// its own, so the value has to be exactly this one.
const (
	attemptSucceeded = "succeeded"
	attemptCancelled = "cancelled"
)

// Chat fabricates conversations about a published analysis. Answers are
// assembled from the analysis the data directory already holds, so citations
// and the pinned source repository are real and the chat-to-fix controls become
// reachable.
type Chat struct {
	opts Options

	mu       sync.Mutex
	sessions map[string]*chatSession
}

type chatSession struct {
	view     analysischat.SessionView
	analysis publishedAnalysis
	// cancelled records request ids the maintainer abandoned, so the turn that
	// is still sleeping resolves as cancelled instead of answering.
	cancelled map[string]bool
}

func newChat(opts Options) *Chat {
	return &Chat{opts: opts, sessions: map[string]*chatSession{}}
}

// Create opens a conversation about one published analysis.
func (c *Chat) Create(ref analysischat.AnalysisRef, login, _ string) (analysischat.SessionView, error) {
	return c.create(ref, login, false)
}

// CreatePrepared opens a conversation seeded with the published conclusion.
func (c *Chat) CreatePrepared(ref analysischat.AnalysisRef, login, _ string) (analysischat.SessionView, error) {
	return c.create(ref, login, true)
}

// PreparedAvailable mirrors the real service for the collapsed cause
// indicator. CreatePrepared always succeeds here, so every cause qualifies.
func (c *Chat) PreparedAvailable(refs []analysischat.AnalysisRef) []bool {
	available := make([]bool, len(refs))
	for i, ref := range refs {
		available[i] = ref.Scope == analysischat.ScopeCause && strings.TrimSpace(ref.JobID) != ""
	}
	return available
}

func (c *Chat) create(ref analysischat.AnalysisRef, login string, prepared bool) (analysischat.SessionView, error) {
	if strings.TrimSpace(ref.JobID) == "" {
		return analysischat.SessionView{}, analysischat.ErrInvalidRequest
	}
	analysis := lookupAnalysis(c.opts.DataDir, ref.JobID, ref.BuildID, ref.TestName)
	now := c.opts.now()
	session := &chatSession{
		analysis:  analysis,
		cancelled: map[string]bool{},
		view: analysischat.SessionView{
			ID: newID(), CreatedBy: login, Analysis: ref,
			CreatedAt: timestamp(now), UpdatedAt: timestamp(now),
			ExpiresAt:        timestamp(now.Add(chatSessionTTL)),
			Messages:         []analysischat.Message{},
			Attempts:         []analysischat.Attempt{},
			MaxTurns:         chatMaxTurns,
			SourceRepository: analysis.Repository,
		},
	}
	if prepared {
		message := c.reply(analysis, "", now)
		message.Prepared = true
		message.Content = "The published conclusion for this failure is reproduced below. " +
			"Ask a follow-up question to probe it.\n\n" + message.Content
		session.view.Messages = append(session.view.Messages, message)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[session.view.ID] = session
	return session.view, nil
}

// Find returns the caller's existing conversation about one analysis.
func (c *Chat) Find(ref analysischat.AnalysisRef, login string) (analysischat.SessionView, error) {
	key := refKey(ref)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, session := range c.sessions {
		if refKey(session.view.Analysis) == key && strings.EqualFold(session.view.CreatedBy, login) {
			return session.view, nil
		}
	}
	return analysischat.SessionView{}, analysischat.ErrSessionNotFound
}

// Get returns one conversation the caller owns.
func (c *Chat) Get(id, login string) (analysischat.SessionView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.ownedLocked(id, login)
	if err != nil {
		return analysischat.SessionView{}, err
	}
	return session.view, nil
}

// Delete discards one conversation the caller owns.
func (c *Chat) Delete(id, login string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.ownedLocked(id, login); err != nil {
		return err
	}
	delete(c.sessions, id)
	return nil
}

// Send answers one question after a simulated model call.
func (c *Chat) Send(ctx context.Context, id, login, requestID, question string) (analysischat.SessionView, error) {
	return c.turn(ctx, id, login, requestID, question, nil)
}

// Stream answers one question, reporting the phases the real turn moves
// through so the streaming UI has something to render.
func (c *Chat) Stream(
	ctx context.Context, id, login, requestID, question string,
	emit func(analysischat.Progress) error,
) (analysischat.SessionView, error) {
	return c.turn(ctx, id, login, requestID, question, emit)
}

// Cancel abandons an in-flight turn.
func (c *Chat) Cancel(id, login, requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.ownedLocked(id, login)
	if err != nil {
		return err
	}
	if session.view.Active == nil || session.view.Active.RequestID != requestID {
		return analysischat.ErrRequestNotFound
	}
	session.cancelled[requestID] = true
	session.view.Active.Phase = analysischat.PhaseCancelling
	session.view.Active.UpdatedAt = timestamp(c.opts.now())
	return nil
}

// turn runs one conversation turn. The session is unlocked while the simulated
// model call sleeps so a concurrent status read or cancellation is not blocked
// behind it.
func (c *Chat) turn(
	ctx context.Context, id, login, requestID, question string,
	emit func(analysischat.Progress) error,
) (analysischat.SessionView, error) {
	if strings.TrimSpace(question) == "" {
		return analysischat.SessionView{}, analysischat.ErrInvalidRequest
	}
	if err := c.beginTurn(id, login, requestID, question); err != nil {
		return analysischat.SessionView{}, err
	}
	phases := []string{
		analysischat.PhaseQueued, analysischat.PhaseInvestigating,
		analysischat.PhaseReadingEvidence, analysischat.PhaseEvaluating, analysischat.PhaseFinalizing,
	}
	step := c.opts.latency() / time.Duration(len(phases))
	for _, phase := range phases {
		if err := c.progress(ctx, id, requestID, phase, step, emit); err != nil {
			c.abandonTurn(id, requestID)
			return analysischat.SessionView{}, err
		}
	}
	return c.completeTurn(id, requestID, question)
}

// beginTurn records the user's question and marks the session busy.
func (c *Chat) beginTurn(id, login, requestID, question string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.ownedLocked(id, login)
	if err != nil {
		return err
	}
	if session.view.Active != nil {
		return analysischat.ErrSessionBusy
	}
	if session.view.TurnsUsed >= session.view.MaxTurns {
		return analysischat.ErrTurnLimit
	}
	now := c.opts.now()
	session.view.Messages = append(session.view.Messages, analysischat.Message{
		Role: "user", Actor: login, RequestID: requestID,
		Content: question, CreatedAt: timestamp(now),
	})
	session.view.Active = &analysischat.ActiveTurn{
		Actor: login, RequestID: requestID, Question: question,
		Phase: analysischat.PhaseQueued, StartedAt: timestamp(now), UpdatedAt: timestamp(now),
	}
	session.view.UpdatedAt = timestamp(now)
	return nil
}

// progress advances one phase, publishing it to a streaming caller.
func (c *Chat) progress(
	ctx context.Context, id, requestID, phase string, step time.Duration,
	emit func(analysischat.Progress) error,
) error {
	timer := time.NewTimer(step)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}

	c.mu.Lock()
	session := c.sessions[id]
	if session == nil || session.view.Active == nil || session.view.Active.RequestID != requestID {
		c.mu.Unlock()
		return analysischat.ErrRequestNotFound
	}
	if session.cancelled[requestID] {
		c.mu.Unlock()
		return context.Canceled
	}
	now := c.opts.now()
	session.view.Active.Phase = phase
	session.view.Active.UpdatedAt = timestamp(now)
	update := analysischat.Progress{
		RequestID: requestID, Phase: phase,
		StartedAt: session.view.Active.StartedAt, UpdatedAt: timestamp(now),
		TurnsUsed: session.view.TurnsUsed, MaxTurns: session.view.MaxTurns,
	}
	c.mu.Unlock()

	if emit == nil {
		return nil
	}
	return emit(update)
}

// abandonTurn clears an in-flight turn a cancellation or disconnect ended.
func (c *Chat) abandonTurn(id, requestID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session := c.sessions[id]
	if session == nil || session.view.Active == nil || session.view.Active.RequestID != requestID {
		return
	}
	session.view.Attempts = append(session.view.Attempts, analysischat.Attempt{
		RequestID: requestID, Actor: session.view.Active.Actor, Question: session.view.Active.Question,
		Outcome: attemptCancelled, Turn: session.view.TurnsUsed + 1,
		CreatedAt: session.view.Active.StartedAt, UpdatedAt: timestamp(c.opts.now()),
	})
	session.view.Active = nil
	delete(session.cancelled, requestID)
}

// completeTurn appends the answer and clears the in-flight turn.
func (c *Chat) completeTurn(id, requestID, question string) (analysischat.SessionView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session := c.sessions[id]
	if session == nil || session.view.Active == nil || session.view.Active.RequestID != requestID {
		return analysischat.SessionView{}, analysischat.ErrRequestNotFound
	}
	now := c.opts.now()
	message := c.reply(session.analysis, question, now)
	message.RequestID = requestID
	session.view.Messages = append(session.view.Messages, message)
	session.view.Attempts = append(session.view.Attempts, analysischat.Attempt{
		RequestID: requestID, Actor: session.view.Active.Actor, Question: question,
		Outcome: attemptSucceeded, Turn: session.view.TurnsUsed + 1,
		CreatedAt: session.view.Active.StartedAt, UpdatedAt: timestamp(now),
	})
	session.view.TurnsUsed++
	session.view.Active = nil
	session.view.UpdatedAt = timestamp(now)
	delete(session.cancelled, requestID)
	return session.view, nil
}

func (c *Chat) ownedLocked(id, login string) (*chatSession, error) {
	session, ok := c.sessions[id]
	if !ok || !strings.EqualFold(session.view.CreatedBy, login) {
		return nil, analysischat.ErrSessionNotFound
	}
	return session, nil
}

// reply assembles one answer. Citations name files the published analysis
// already verified, which is what the chat-to-fix gate looks for.
func (c *Chat) reply(analysis publishedAnalysis, question string, now time.Time) analysischat.Message {
	var b strings.Builder
	b.WriteString("This answer came from the local mock server, so no model was called.\n\n")
	if question != "" {
		b.WriteString("**You asked:** " + truncate(question, 200) + "\n\n")
	}
	b.WriteString("**Published root cause**\n\n" + rootCauseOrPlaceholder(analysis) + "\n")

	paths := citedPaths(analysis.FileLinks)
	citations := make([]analysischat.Citation, 0, len(paths))
	if len(paths) > 0 {
		b.WriteString("\n**Evidence read**\n\n")
		for _, path := range paths {
			fmt.Fprintf(&b, "- `%s`\n", path)
			citations = append(citations, analysischat.Citation{
				Path: path, LineStart: 1, LineEnd: 20,
				Quote: "mock excerpt from " + path,
			})
		}
	}

	message := analysischat.Message{
		Role: "assistant", Content: b.String(), Assessment: "supports",
		Citations: citations, CreatedAt: timestamp(now),
		ToolCalls: len(paths) + 2, GCSBytes: 48_000,
		ElapsedMs: int(c.opts.latency() / time.Millisecond), ProviderMs: 900,
	}
	if len(citations) > 0 {
		message.ProposedRevision = &analysischat.Revision{
			RootCause:    "Mock revision: " + truncate(rootCauseOrPlaceholder(analysis), 160),
			SuggestedFix: "Wait for the condition to settle before asserting on it.",
		}
	} else {
		message.Assessment = "inconclusive"
		message.EvidenceWarnings = []string{"No verified source files were published for this analysis."}
	}
	return message
}

// refKey identifies the analysis a session is about.
func refKey(ref analysischat.AnalysisRef) string {
	return strings.Join([]string{
		ref.Scope, ref.JobID, ref.BuildID, ref.TestName,
		ref.PatternID, ref.CausalGroupID,
	}, "\x00")
}
