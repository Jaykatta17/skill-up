// Package observation records privacy-aware Skill usage observations and turns
// approved observations into candidate evaluation cases.
package observation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	observationIDPattern = regexp.MustCompile(`^obs_[a-f0-9]{24}$`)
	skillNamePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

const (
	// SchemaVersion is the current observation document schema.
	SchemaVersion = "v1alpha1"

	// AttributionExplicit identifies a Skill named directly by the user.
	AttributionExplicit = "explicit"
	// AttributionInstrumented identifies a Skill declared by the host adapter.
	AttributionInstrumented = "instrumented"
	// AttributionInferred is reserved and cannot be persisted.
	AttributionInferred = "inferred"

	// ReviewCandidate marks an observation that has not been reviewed.
	ReviewCandidate = "candidate"
	// ReviewApproved marks an observation approved for case conversion.
	ReviewApproved = "approved"
	// ReviewRejected marks an observation rejected for case conversion.
	ReviewRejected = "rejected"
)

// Observation is a normalized, locally stored record of a Skill interaction.
type Observation struct {
	SchemaVersion string      `json:"schema_version"`
	ID            string      `json:"id"`
	Host          Host        `json:"host"`
	Skill         Skill       `json:"skill"`
	Attribution   Attribution `json:"attribution"`
	Input         Content     `json:"input"`
	Outcome       Outcome     `json:"outcome"`
	Evidence      []Evidence  `json:"evidence,omitempty"`
	Feedback      *Feedback   `json:"feedback,omitempty"`
	Correlation   Correlation `json:"correlation"`
	Timing        Timing      `json:"timing"`
	Privacy       Privacy     `json:"privacy"`
	Review        Review      `json:"review"`
}

// Host identifies the Agent host and adapter that emitted an observation.
type Host struct {
	Name           string `json:"name"`
	Version        string `json:"version,omitempty"`
	AdapterVersion string `json:"adapter_version,omitempty"`
}

// Skill identifies the observed Skill.
type Skill struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Attribution explains how the observation was connected to a Skill.
type Attribution struct {
	Method     string   `json:"method"`
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence,omitempty"`
}

// Content holds either inline text or a reference.
type Content struct {
	Text string `json:"text,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

// Outcome describes how the observed interaction ended.
type Outcome struct {
	Status       string `json:"status"`
	FinalMessage string `json:"final_message,omitempty"`
}

// Evidence is a redacted reference attached to an observation.
type Evidence struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Feedback records explicit user feedback about the Skill interaction.
type Feedback struct {
	Sentiment string `json:"sentiment,omitempty"`
	Comment   string `json:"comment,omitempty"`
}

// Correlation maps an observation to host session and turn identifiers.
type Correlation struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id,omitempty"`
}

// Timing records when the interaction was observed and completed.
type Timing struct {
	ObservedAt  time.Time  `json:"observed_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// Privacy records local storage, consent, and applied redactions.
type Privacy struct {
	Storage    string   `json:"storage"`
	Consent    string   `json:"consent"`
	Redactions []string `json:"redactions,omitempty"`
}

// Review tracks the human decision for case conversion.
type Review struct {
	Status     string     `json:"status"`
	ReviewedAt *time.Time `json:"reviewed_at,omitempty"`
}

// Validate rejects observations that cannot be safely attributed and reviewed.
func (o *Observation) Validate() error {
	errs := o.identityErrors()
	errs = append(errs, o.semanticErrors()...)
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (o *Observation) identityErrors() []string {
	var errs []string
	if o.SchemaVersion != SchemaVersion {
		errs = append(errs, "schema_version must be v1alpha1")
	}
	if !observationIDPattern.MatchString(o.ID) {
		errs = append(errs, "id must match obs_<24 lowercase hex characters>")
	}
	if o.Host.Name == "" {
		errs = append(errs, "host.name is required")
	}
	if !skillNamePattern.MatchString(o.Skill.Name) {
		errs = append(errs, "skill.name must use 1-64 lowercase letters, digits, underscores, or hyphens")
	}
	if o.Correlation.SessionID == "" {
		errs = append(errs, "correlation.session_id is required")
	}
	if o.Input.Text == "" && o.Input.Ref == "" {
		errs = append(errs, "input.text or input.ref is required")
	}
	return errs
}

func (o *Observation) semanticErrors() []string {
	var errs []string
	if o.Outcome.Status != "completed" && o.Outcome.Status != "interrupted" {
		errs = append(errs, "outcome.status must be completed or interrupted")
	}
	if err := validateAttributionMethod(o.Attribution.Method); err != nil {
		errs = append(errs, err.Error())
	}
	if o.Attribution.Confidence < 0 || o.Attribution.Confidence > 1 {
		errs = append(errs, "attribution.confidence must be between 0 and 1")
	}
	if o.Timing.ObservedAt.IsZero() {
		errs = append(errs, "timing.observed_at is required")
	}
	if o.Privacy.Storage != "local" || o.Privacy.Consent == "" {
		errs = append(errs, "privacy.storage must be local and privacy.consent is required")
	}
	if !validReviewStatus(o.Review.Status) {
		errs = append(errs, "review.status must be candidate, approved, or rejected")
	}
	for _, evidence := range o.Evidence {
		if strings.TrimSpace(evidence.Kind) == "" {
			errs = append(errs, "evidence.kind is required")
			break
		}
	}
	if o.Feedback != nil && !validFeedbackSentiment(o.Feedback.Sentiment) {
		errs = append(errs, "feedback.sentiment must be positive, negative, mixed, neutral, or empty")
	}
	return errs
}

func validFeedbackSentiment(sentiment string) bool {
	return sentiment == "" || sentiment == "positive" || sentiment == "negative" || sentiment == "mixed" || sentiment == "neutral"
}

func validateAttributionMethod(method string) error {
	switch method {
	case AttributionExplicit, AttributionInstrumented:
		return nil
	case AttributionInferred:
		return errors.New("inferred attribution cannot be persisted as an observation")
	default:
		return errors.New("attribution.method must be explicit or instrumented")
	}
}

func validReviewStatus(status string) bool {
	return status == ReviewCandidate || status == ReviewApproved || status == ReviewRejected
}

// AssignID derives a stable ID from the semantic observation fields. This
// intentionally excludes timestamps and review state so repeated hook delivery
// is de-duplicated.
func (o *Observation) AssignID() {
	material := strings.Join([]string{
		o.Host.Name,
		o.Skill.Name,
		o.Skill.Version,
		o.Attribution.Method,
		o.Input.Text,
		o.Input.Ref,
		o.Outcome.Status,
		o.Outcome.FinalMessage,
		o.Correlation.SessionID,
		o.Correlation.TurnID,
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	o.ID = "obs_" + hex.EncodeToString(sum[:12])
}

// Summary returns a compact human-readable description.
func (o *Observation) Summary() string {
	return fmt.Sprintf("%s\t%s\t%s\t%s", o.ID, o.Skill.Name, o.Review.Status, o.Outcome.Status)
}
