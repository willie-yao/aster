package causalcritic

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	preflightPendingRetention  = time.Hour
	preflightEvidenceRetention = 6 * time.Hour
	preflightAttemptRetention  = 30 * 24 * time.Hour
)

// PreflightStatus classifies dashboard work before a model request is made.
type PreflightStatus string

const (
	PreflightPending        PreflightStatus = "pending"
	PreflightEvidenceFailed PreflightStatus = "evidence_failed"
	PreflightInputInvalid   PreflightStatus = "input_invalid"
	PreflightSubmitted      PreflightStatus = "submitted"
)

// PreflightAttempt is a compact durable candidate identity and pre-model outcome.
type PreflightAttempt struct {
	Hash             string          `json:"hash"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
	Status           PreflightStatus `json:"status"`
	FailureCode      string          `json:"failure_code,omitempty"`
	TrialAttemptHash string          `json:"trial_attempt_hash,omitempty"`
}

// PreflightIdentityInput contains immutable values available before artifact I/O.
type PreflightIdentityInput struct {
	RequestHash       string
	AuthoritativeHash string
	SourceRevision    string
	SkillHash         string
	RuntimeIdentity   string
}

// PreflightIdentity seals one candidate before evidence retrieval.
func PreflightIdentity(input PreflightIdentityInput) (string, error) {
	skillHash := strings.TrimSpace(input.SkillHash)
	if skillHash == "" {
		skillHash = hashString("")
	}
	for name, value := range map[string]string{
		"request": input.RequestHash, "authoritative": input.AuthoritativeHash,
		"skill": skillHash, "runtime": input.RuntimeIdentity,
	} {
		if !validSHA256(value) {
			return "", fmt.Errorf("causal critic preflight %s hash is invalid", name)
		}
	}
	if len(input.SourceRevision) != 40 || input.SourceRevision != strings.ToLower(input.SourceRevision) {
		return "", fmt.Errorf("causal critic preflight source revision is invalid")
	}
	if _, err := hex.DecodeString(input.SourceRevision); err != nil {
		return "", fmt.Errorf("causal critic preflight source revision is invalid")
	}
	data, _ := json.Marshal(struct {
		ContractVersion   string `json:"contract_version"`
		RequestHash       string `json:"request_hash"`
		AuthoritativeHash string `json:"authoritative_hash"`
		SourceRevision    string `json:"source_revision"`
		SkillHash         string `json:"skill_hash"`
		RuntimeIdentity   string `json:"runtime_identity"`
	}{ContractVersion, input.RequestHash, input.AuthoritativeHash, input.SourceRevision, skillHash, input.RuntimeIdentity})
	return hashString(string(data)), nil
}

// ClaimPreflightAttempt atomically reserves one candidate before artifact I/O.
func ClaimPreflightAttempt(publicDir, ledgerPath, identity string, now time.Time) (PreflightAttempt, bool, error) {
	if !validSHA256(identity) {
		return PreflightAttempt{}, false, fmt.Errorf("causal critic preflight identity is invalid")
	}
	now = now.UTC()
	attempt := PreflightAttempt{
		Hash: identity, CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano), Status: PreflightPending,
	}
	claimed := false
	err := withLedgerLock(publicDir, ledgerPath, func(path string) error {
		ledger, err := loadLedger(path)
		if err != nil {
			return err
		}
		pruneLedger(&ledger, now)
		for _, existing := range ledger.Preflights {
			if existing.Hash == identity && preflightActive(existing, now) {
				attempt = existing
				return nil
			}
		}
		upsertPreflight(&ledger, attempt)
		ledger.UpdatedAt = attempt.UpdatedAt
		if err := writeLedger(path, ledger); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return attempt, claimed, err
}

// CompletePreflightAttempt records one bounded pre-model or submitted outcome.
func CompletePreflightAttempt(publicDir, ledgerPath, identity string, status PreflightStatus, failureCode, trialAttemptHash string, now time.Time) error {
	if !validSHA256(identity) {
		return fmt.Errorf("causal critic preflight identity is invalid")
	}
	now = now.UTC()
	return withLedgerLock(publicDir, ledgerPath, func(path string) error {
		ledger, err := loadLedger(path)
		if err != nil {
			return err
		}
		for index := range ledger.Preflights {
			attempt := &ledger.Preflights[index]
			if attempt.Hash != identity {
				continue
			}
			if attempt.Status != PreflightPending {
				return fmt.Errorf("causal critic preflight attempt is already complete")
			}
			attempt.Status = status
			attempt.UpdatedAt = now.Format(time.RFC3339Nano)
			attempt.FailureCode = strings.TrimSpace(failureCode)
			attempt.TrialAttemptHash = strings.TrimSpace(trialAttemptHash)
			if err := validatePreflightAttempt(*attempt); err != nil {
				return err
			}
			ledger.UpdatedAt = attempt.UpdatedAt
			return writeLedger(path, ledger)
		}
		return fmt.Errorf("causal critic preflight claim is missing")
	})
}

func validatePreflightAttempt(attempt PreflightAttempt) error {
	if !validSHA256(attempt.Hash) {
		return fmt.Errorf("invalid causal critic preflight hash")
	}
	created, err := time.Parse(time.RFC3339Nano, attempt.CreatedAt)
	if err != nil {
		return fmt.Errorf("invalid causal critic preflight creation time: %w", err)
	}
	updated, err := time.Parse(time.RFC3339Nano, attempt.UpdatedAt)
	if err != nil || updated.Before(created) {
		return fmt.Errorf("invalid causal critic preflight update time")
	}
	switch attempt.Status {
	case PreflightPending:
		if attempt.FailureCode != "" || attempt.TrialAttemptHash != "" {
			return fmt.Errorf("pending causal critic preflight contains a result")
		}
	case PreflightEvidenceFailed, PreflightInputInvalid:
		if !failureCodeRE.MatchString(attempt.FailureCode) || attempt.TrialAttemptHash != "" {
			return fmt.Errorf("failed causal critic preflight result is invalid")
		}
	case PreflightSubmitted:
		if attempt.FailureCode != "" && !failureCodeRE.MatchString(attempt.FailureCode) {
			return fmt.Errorf("submitted causal critic preflight failure code is invalid")
		}
		if !validSHA256(attempt.TrialAttemptHash) {
			return fmt.Errorf("submitted causal critic preflight trial identity is invalid")
		}
	default:
		return fmt.Errorf("unsupported causal critic preflight status %q", attempt.Status)
	}
	return nil
}

func preflightActive(attempt PreflightAttempt, reference time.Time) bool {
	updated, err := time.Parse(time.RFC3339Nano, attempt.UpdatedAt)
	if err != nil {
		return false
	}
	retention := preflightAttemptRetention
	switch attempt.Status {
	case PreflightPending:
		retention = preflightPendingRetention
	case PreflightEvidenceFailed:
		retention = preflightEvidenceRetention
	}
	return !updated.Before(reference.Add(-retention))
}

func upsertPreflight(ledger *Ledger, attempt PreflightAttempt) {
	for index := range ledger.Preflights {
		if ledger.Preflights[index].Hash == attempt.Hash {
			ledger.Preflights[index] = attempt
			return
		}
	}
	ledger.Preflights = append(ledger.Preflights, attempt)
}
