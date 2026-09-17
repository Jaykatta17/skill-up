package observation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const observerSkillName = "skill-up-observer"

var explicitSkillPattern = regexp.MustCompile(`(?:^|\s)\$([a-zA-Z0-9][a-zA-Z0-9_-]*)`)

// HookInput is the stable subset of the Codex hook JSON payload consumed by
// the observer. Unknown fields are intentionally ignored for forward
// compatibility.
type HookInput struct {
	SessionID            string          `json:"session_id"`
	TurnID               string          `json:"turn_id"`
	HookEventName        string          `json:"hook_event_name"`
	Model                string          `json:"model"`
	Prompt               string          `json:"prompt"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	LastAssistantMessage string          `json:"last_assistant_message"`
}

type draft struct {
	SessionID   string      `json:"session_id"`
	TurnID      string      `json:"turn_id,omitempty"`
	Model       string      `json:"model,omitempty"`
	Prompt      string      `json:"prompt,omitempty"`
	Skill       Skill       `json:"skill"`
	Attribution Attribution `json:"attribution"`
	Evidence    []Evidence  `json:"evidence,omitempty"`
	Feedback    *Feedback   `json:"feedback,omitempty"`
	Redactions  []string    `json:"redactions,omitempty"`
	ObservedAt  time.Time   `json:"observed_at"`
}

// HookHandler normalizes Codex lifecycle hook payloads into observations.
type HookHandler struct {
	store *Store
	now   func() time.Time
}

// NewHookHandler creates a handler backed by a local observation store.
func NewHookHandler(store *Store) *HookHandler {
	return &HookHandler{store: store, now: nowUTC}
}

// Handle consumes one Codex hook payload. It returns a newly finalized
// observation, if the event completed an attributable interaction.
func (h *HookHandler) Handle(in HookInput) (*Observation, error) {
	if in.SessionID == "" {
		return nil, errors.New("hook payload session_id is required")
	}
	unlock, err := acquireFileLock(h.draftPath(in.SessionID, in.TurnID)+".lock", 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer unlock()
	switch in.HookEventName {
	case "UserPromptSubmit":
		return nil, h.onPrompt(in)
	case "PostToolUse":
		return nil, h.onTool(in)
	case "Stop":
		return h.finalize(in, "completed")
	case "Interrupt":
		return h.finalize(in, "interrupted")
	default:
		return nil, fmt.Errorf("unsupported hook event %q", in.HookEventName)
	}
}

func (h *HookHandler) onPrompt(in HookInput) error {
	prompt, redactions := Redact(in.Prompt, nil)
	d := &draft{
		SessionID:  in.SessionID,
		TurnID:     in.TurnID,
		Model:      in.Model,
		Prompt:     prompt,
		ObservedAt: h.now(),
		Redactions: redactions,
	}
	for _, match := range explicitSkillPattern.FindAllStringSubmatch(prompt, -1) {
		name := strings.ToLower(match[1])
		if name == observerSkillName || !skillNamePattern.MatchString(name) {
			continue
		}
		d.Skill.Name = name
		d.Attribution = Attribution{
			Method:     AttributionExplicit,
			Confidence: 1,
			Evidence:   []string{"user prompt explicitly referenced $" + name},
		}
		break
	}
	return h.saveDraft(d)
}

func (h *HookHandler) onTool(in HookInput) error {
	d, err := h.loadDraft(in.SessionID, in.TurnID)
	if err != nil {
		return err
	}
	if d == nil {
		d = &draft{SessionID: in.SessionID, TurnID: in.TurnID, Model: in.Model, ObservedAt: h.now()}
	}
	tool := shortToolName(in.ToolName)
	switch tool {
	case "mark_skill_invocation":
		err = applySkillMarker(d, in.ToolInput)
	case "attach_skill_evidence":
		err = applyEvidenceMarker(d, in.ToolInput)
	case "record_skill_feedback":
		err = applyFeedbackMarker(d, in.ToolInput)
	default:
		return fmt.Errorf("unsupported observer tool %q", in.ToolName)
	}
	if err != nil {
		return err
	}
	return h.saveDraft(d)
}

func applySkillMarker(d *draft, input json.RawMessage) error {
	var args struct {
		SkillName    string `json:"skill_name"`
		SkillVersion string `json:"skill_version"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Errorf("parse mark_skill_invocation input: %w", err)
	}
	name := strings.ToLower(strings.TrimSpace(args.SkillName))
	if !skillNamePattern.MatchString(name) {
		return errors.New("mark_skill_invocation skill_name must use 1-64 lowercase letters, digits, underscores, or hyphens")
	}
	version, redactions := Redact(strings.TrimSpace(args.SkillVersion), nil)
	d.Redactions = mergeStrings(d.Redactions, redactions)
	d.Skill = Skill{Name: name, Version: version}
	d.Attribution = Attribution{
		Method:     AttributionInstrumented,
		Confidence: 1,
		Evidence:   []string{"Skill invocation marked through the observer MCP tool"},
	}
	return nil
}

func applyEvidenceMarker(d *draft, input json.RawMessage) error {
	var args struct {
		Kind      string `json:"kind"`
		Reference string `json:"reference"`
		Summary   string `json:"summary"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Errorf("parse attach_skill_evidence input: %w", err)
	}
	kind, r0 := Redact(strings.TrimSpace(args.Kind), nil)
	if kind == "" {
		return errors.New("attach_skill_evidence requires kind")
	}
	ref, r1 := Redact(args.Reference, nil)
	summary, r2 := Redact(args.Summary, nil)
	d.Redactions = mergeStrings(d.Redactions, append(r0, append(r1, r2...)...))
	d.Evidence = append(d.Evidence, Evidence{Kind: kind, Ref: ref, Summary: summary})
	return nil
}

func applyFeedbackMarker(d *draft, input json.RawMessage) error {
	var args Feedback
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Errorf("parse record_skill_feedback input: %w", err)
	}
	if !validFeedbackSentiment(args.Sentiment) {
		return errors.New("record_skill_feedback sentiment must be positive, negative, mixed, or neutral")
	}
	args.Comment, d.Redactions = redactAndMerge(args.Comment, d.Redactions)
	d.Feedback = &args
	return nil
}

func (h *HookHandler) finalize(in HookInput, status string) (*Observation, error) {
	d, err := h.loadDraft(in.SessionID, in.TurnID)
	if err != nil {
		return nil, err
	}
	defer h.removeDraft(in.SessionID, in.TurnID)
	if d == nil || d.Skill.Name == "" || (d.Attribution.Method != AttributionExplicit && d.Attribution.Method != AttributionInstrumented) {
		return nil, nil
	}
	message, redactions := Redact(in.LastAssistantMessage, nil)
	completedAt := h.now()
	o := &Observation{
		SchemaVersion: SchemaVersion,
		Host:          Host{Name: "codex", Version: d.Model, AdapterVersion: "v1alpha1"},
		Skill:         d.Skill,
		Attribution:   d.Attribution,
		Input:         Content{Text: d.Prompt},
		Outcome:       Outcome{Status: status, FinalMessage: message},
		Evidence:      d.Evidence,
		Feedback:      d.Feedback,
		Correlation:   Correlation{SessionID: d.SessionID, TurnID: d.TurnID},
		Timing:        Timing{ObservedAt: d.ObservedAt, CompletedAt: &completedAt},
		Privacy: Privacy{
			Storage:    "local",
			Consent:    "codex_plugin_enabled_and_hooks_trusted",
			Redactions: mergeStrings(d.Redactions, redactions),
		},
		Review: Review{Status: ReviewCandidate},
	}
	o.AssignID()
	created, err := h.store.Save(o)
	if err != nil {
		return nil, err
	}
	if !created {
		return h.store.Load(o.ID)
	}
	return o, nil
}

func shortToolName(name string) string {
	if index := strings.LastIndex(name, "__"); index >= 0 {
		return name[index+2:]
	}
	return name
}

func redactAndMerge(value string, existing []string) (string, []string) {
	redacted, names := Redact(value, nil)
	return redacted, mergeStrings(existing, names)
}

func mergeStrings(left, right []string) []string {
	seen := make(map[string]struct{}, len(left)+len(right))
	result := make([]string, 0, len(left)+len(right))
	for _, item := range append(left, right...) {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	return result
}

func (h *HookHandler) draftPath(sessionID, turnID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + turnID))
	return filepath.Join(h.store.root, ".drafts", hex.EncodeToString(sum[:16])+".json")
}

func (h *HookHandler) saveDraft(d *draft) error {
	dir := filepath.Join(h.store.root, ".drafts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create observation draft directory: %w", err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	path := h.draftPath(d.SessionID, d.TurnID)
	tmp, err := os.CreateTemp(dir, ".draft-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (h *HookHandler) loadDraft(sessionID, turnID string) (*draft, error) {
	data, err := os.ReadFile(h.draftPath(sessionID, turnID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d draft
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (h *HookHandler) removeDraft(sessionID, turnID string) {
	_ = os.Remove(h.draftPath(sessionID, turnID))
}
