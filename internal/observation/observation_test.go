package observation

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testObservation() *Observation {
	o := &Observation{
		SchemaVersion: SchemaVersion,
		Host:          Host{Name: "codex"},
		Skill:         Skill{Name: "demo-skill"},
		Attribution:   Attribution{Method: AttributionExplicit, Confidence: 1},
		Input:         Content{Text: "Run the demo"},
		Outcome:       Outcome{Status: "completed", FinalMessage: "done"},
		Correlation:   Correlation{SessionID: "session-1", TurnID: "turn-1"},
		Timing:        Timing{ObservedAt: time.Date(2026, 9, 17, 1, 2, 3, 0, time.UTC)},
		Privacy:       Privacy{Storage: "local", Consent: "test"},
		Review:        Review{Status: ReviewCandidate},
	}
	o.AssignID()
	return o
}

func TestRedactCommonCredentials(t *testing.T) {
	t.Parallel()
	input := "Authorization: Bearer abcdefghijklmnop token=secret-value key sk-abcdefghijklmnopqrstuvwxyz"
	got, categories := Redact(input, []string{"secret-value"})
	if strings.Contains(got, "abcdefghijklmnop") || strings.Contains(got, "secret-value") || strings.Contains(got, "sk-abc") {
		t.Fatalf("Redact() leaked a credential: %q", got)
	}
	if len(categories) < 2 {
		t.Fatalf("Redact() categories = %v, want at least two categories", categories)
	}
}

func TestCodexObservationFixtureMatchesContract(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "codex-explicit-observation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o Observation
	if err := json.Unmarshal(data, &o); err != nil {
		t.Fatal(err)
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("fixture validation failed: %v", err)
	}
	if o.Host.Name != "codex" || o.Attribution.Method != AttributionExplicit {
		t.Fatalf("fixture identity = host %q, attribution %q", o.Host.Name, o.Attribution.Method)
	}
}

func TestStoreSaveDeduplicatesWithoutOverwrite(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	o := testObservation()
	created, err := store.Save(o)
	if err != nil || !created {
		t.Fatalf("first Save() = (%v, %v), want created", created, err)
	}
	original, err := os.ReadFile(filepath.Join(store.root, o.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	o.Outcome.FinalMessage = "different"
	created, err = store.Save(o)
	if err != nil || created {
		t.Fatalf("second Save() = (%v, %v), want duplicate", created, err)
	}
	after, err := os.ReadFile(filepath.Join(store.root, o.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("duplicate Save() overwrote the original observation")
	}
	info, err := os.Stat(filepath.Join(store.root, o.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("observation mode = %o, want 600", got)
	}
}

func TestHookHandlerExplicitLifecycleAndDedupe(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	h := NewHookHandler(store)
	fixed := time.Date(2026, 9, 17, 2, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return fixed }
	_, err := h.Handle(HookInput{
		SessionID: "s1", TurnID: "t1", HookEventName: "UserPromptSubmit",
		Prompt: "Use $demo-skill with token=my-secret-value", Model: "gpt-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	o, err := h.Handle(HookInput{
		SessionID: "s1", TurnID: "t1", HookEventName: "Stop",
		LastAssistantMessage: "completed using sk-abcdefghijklmnopqrstuvwxyz",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o == nil || o.Skill.Name != "demo-skill" || o.Attribution.Method != AttributionExplicit || o.Host.Version != "gpt-test" {
		t.Fatalf("final observation = %#v", o)
	}
	encoded, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "my-secret") || strings.Contains(string(encoded), "sk-abc") {
		t.Fatalf("observation leaked secret: %s", encoded)
	}
	items, err := store.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("List() = (%d items, %v), want one", len(items), err)
	}
}

func TestHookHandlerInstrumentedAndUnattributed(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	h := NewHookHandler(store)
	_, err := h.Handle(HookInput{SessionID: "s2", TurnID: "t1", HookEventName: "UserPromptSubmit", Prompt: "Help me"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.Handle(HookInput{SessionID: "s2", TurnID: "t1", HookEventName: "Stop", LastAssistantMessage: "done"})
	if err != nil || result != nil {
		t.Fatalf("unattributed Stop = (%#v, %v), want ignored", result, err)
	}

	_, err = h.Handle(HookInput{SessionID: "s3", TurnID: "t1", HookEventName: "UserPromptSubmit", Prompt: "Help me"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Handle(HookInput{
		SessionID: "s3", TurnID: "t1", HookEventName: "PostToolUse",
		ToolName:  "mcp__skill_up_observer__mark_skill_invocation",
		ToolInput: json.RawMessage(`{"skill_name":"demo-skill","skill_version":"1.2.3"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err = h.Handle(HookInput{SessionID: "s3", TurnID: "t1", HookEventName: "Interrupt"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Attribution.Method != AttributionInstrumented || result.Outcome.Status != "interrupted" {
		t.Fatalf("instrumented observation = %#v", result)
	}
}

func TestHookHandlerSerializesConcurrentMarkers(t *testing.T) {
	t.Parallel()
	store := NewStore(t.TempDir())
	h := NewHookHandler(store)
	if _, err := h.Handle(HookInput{
		SessionID: "concurrent-session", TurnID: "turn-1", HookEventName: "UserPromptSubmit", Prompt: "Help me",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Handle(HookInput{
		SessionID: "concurrent-session", TurnID: "turn-1", HookEventName: "PostToolUse",
		ToolName: "mcp__skill_up_observer__mark_skill_invocation", ToolInput: json.RawMessage(`{"skill_name":"demo-skill"}`),
	}); err != nil {
		t.Fatal(err)
	}

	const markerCount = 20
	errCh := make(chan error, markerCount)
	var wg sync.WaitGroup
	for range markerCount {
		wg.Go(func() {
			_, err := h.Handle(HookInput{
				SessionID: "concurrent-session", TurnID: "turn-1", HookEventName: "PostToolUse",
				ToolName: "mcp__skill_up_observer__attach_skill_evidence", ToolInput: json.RawMessage(`{"kind":"test","summary":"evidence"}`),
			})
			errCh <- err
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	o, err := h.Handle(HookInput{SessionID: "concurrent-session", TurnID: "turn-1", HookEventName: "Stop"})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(o.Evidence); got != markerCount {
		t.Fatalf("evidence count = %d, want %d", got, markerCount)
	}
}

func TestHookHandlerRejectsInvalidMarkerMetadata(t *testing.T) {
	t.Parallel()
	h := NewHookHandler(NewStore(t.TempDir()))
	_, err := h.Handle(HookInput{
		SessionID: "invalid-marker", HookEventName: "PostToolUse",
		ToolName: "mark_skill_invocation", ToolInput: json.RawMessage(`{"skill_name":"../unsafe"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "skill_name") {
		t.Fatalf("invalid skill marker error = %v", err)
	}
	_, err = h.Handle(HookInput{
		SessionID: "invalid-feedback", HookEventName: "PostToolUse",
		ToolName: "record_skill_feedback", ToolInput: json.RawMessage(`{"sentiment":"excellent"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "sentiment") {
		t.Fatalf("invalid feedback marker error = %v", err)
	}
}

func TestWriteCandidateCaseRequiresApprovalAndNeverOverwrites(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "evals", "cases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: demo-skill\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "evals", "eval.yaml"), []byte(`schema_version: v1alpha1
environment:
  type: none
engine:
  name: codex
cases:
  files:
    - evals/cases/existing.yaml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "evals", "cases", "existing.yaml"), []byte("id: existing\ninput:\n  prompt: existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := testObservation()
	if _, err := WriteCandidateCase(o, root); err == nil || !strings.Contains(err.Error(), "approved") {
		t.Fatalf("unapproved WriteCandidateCase() error = %v", err)
	}
	o.Review.Status = ReviewApproved
	path, err := WriteCandidateCase(o, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	evalData, err := os.ReadFile(filepath.Join(root, "evals", "eval.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(evalData), filepath.Base(path)) {
		t.Fatalf("eval.yaml was not updated:\n%s", evalData)
	}
	if _, err := WriteCandidateCase(o, root); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second WriteCandidateCase() error = %v", err)
	}
}

func TestWriteCandidateCaseSerializesConcurrentUpdates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "evals", "cases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: demo-skill\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "evals", "eval.yaml"), []byte(`schema_version: v1alpha1
environment:
  type: none
engine:
  name: codex
cases:
  files: []
`), 0o600); err != nil {
		t.Fatal(err)
	}

	observations := []*Observation{testObservation(), testObservation()}
	observations[0].Input.Text = "first prompt"
	observations[1].Input.Text = "second prompt"
	for _, item := range observations {
		item.Review.Status = ReviewApproved
		item.AssignID()
	}
	errCh := make(chan error, len(observations))
	var wg sync.WaitGroup
	for _, item := range observations {
		wg.Go(func() {
			_, err := WriteCandidateCase(item, root)
			errCh <- err
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	evalData, err := os.ReadFile(filepath.Join(root, "evals", "eval.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range observations {
		_, caseID, err := CandidateCase(item)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(evalData), caseID+".yaml") {
			t.Fatalf("eval.yaml does not reference %s:\n%s", caseID, evalData)
		}
	}
}

func TestServeMCPListsAndCallsMarkerTools(t *testing.T) {
	t.Parallel()
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"mark_skill_invocation\",\"arguments\":{\"skill_name\":\"demo\"}}}\n")
	var output bytes.Buffer
	if err := ServeMCP(input, &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mark_skill_invocation", "attach_skill_evidence", "record_skill_feedback", "accepted"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("MCP output missing %q: %s", want, output.String())
		}
	}
}

func TestValidateToolArgumentsRejectsInvalidMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{name: "unsafe skill name", tool: "mark_skill_invocation", args: map[string]any{"skill_name": "../unsafe"}},
		{name: "empty evidence kind", tool: "attach_skill_evidence", args: map[string]any{"kind": ""}},
		{name: "invalid sentiment", tool: "record_skill_feedback", args: map[string]any{"sentiment": "excellent"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := validateToolArguments(test.tool, test.args); err == nil {
				t.Fatalf("validateToolArguments(%q, %#v) succeeded", test.tool, test.args)
			}
		})
	}
}
