package observation

import (
	"regexp"
	"sort"
	"strings"
)

type redactionRule struct {
	name string
	re   *regexp.Regexp
}

var redactionRules = []redactionRule{
	{name: "bearer_token", re: regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{12,}`)},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[a-zA-Z0-9_-]{8,}\.[a-zA-Z0-9_-]{8,}\.[a-zA-Z0-9_-]{8,}\b`)},
	{name: "provider_key", re: regexp.MustCompile(`\b(?:sk-[a-zA-Z0-9_-]{12,}|gh[pousr]_[a-zA-Z0-9_]{12,}|AIza[a-zA-Z0-9_-]{20,})\b`)},
	{name: "aws_access_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{name: "secret_assignment", re: regexp.MustCompile(`(?i)\b(api[_-]?key|token|password|secret)\s*[:=]\s*[^\s,;]+`)},
}

// Redact replaces common credential shapes and caller-provided literals.
func Redact(value string, literals []string) (string, []string) {
	redacted := value
	found := make(map[string]struct{})
	for _, rule := range redactionRules {
		if rule.re.MatchString(redacted) {
			redacted = rule.re.ReplaceAllString(redacted, "[REDACTED]")
			found[rule.name] = struct{}{}
		}
	}
	for _, literal := range literals {
		if literal == "" || !strings.Contains(redacted, literal) {
			continue
		}
		redacted = strings.ReplaceAll(redacted, literal, "[REDACTED]")
		found["configured_literal"] = struct{}{}
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return redacted, names
}
