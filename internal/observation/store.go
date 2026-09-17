package observation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Store persists observations locally with restrictive permissions.
type Store struct {
	root string
}

// NewStore creates a local observation store rooted at the given path.
func NewStore(root string) *Store {
	return &Store{root: root}
}

// DefaultDataDir returns the configured or conventional observation path.
func DefaultDataDir() (string, error) {
	if configured := os.Getenv("SKILL_UP_OBSERVATION_DIR"); configured != "" {
		return filepath.Abs(configured)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".skill-up", "observations"), nil
}

// Save validates and exclusively creates an observation document.
func (s *Store) Save(o *Observation) (bool, error) {
	if err := o.Validate(); err != nil {
		return false, fmt.Errorf("validate observation: %w", err)
	}
	if !safeID.MatchString(o.ID) {
		return false, fmt.Errorf("unsafe observation id %q", o.ID)
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return false, fmt.Errorf("create observation directory: %w", err)
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshal observation: %w", err)
	}
	path := filepath.Join(s.root, o.ID+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create observation: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("write observation: %w", err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("close observation: %w", err)
	}
	return true, nil
}

// Load reads and validates one observation by ID.
func (s *Store) Load(id string) (*Observation, error) {
	if !safeID.MatchString(id) {
		return nil, fmt.Errorf("unsafe observation id %q", id)
	}
	data, err := os.ReadFile(filepath.Join(s.root, id+".json"))
	if err != nil {
		return nil, fmt.Errorf("read observation %s: %w", id, err)
	}
	var o Observation
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("parse observation %s: %w", id, err)
	}
	if err := o.Validate(); err != nil {
		return nil, fmt.Errorf("invalid observation %s: %w", id, err)
	}
	return &o, nil
}

// List returns all stored observations in reverse chronological order.
func (s *Store) List() ([]Observation, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list observations: %w", err)
	}
	items := make([]Observation, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		o, loadErr := s.Load(entry.Name()[:len(entry.Name())-len(".json")])
		if loadErr != nil {
			return nil, loadErr
		}
		items = append(items, *o)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Timing.ObservedAt.After(items[j].Timing.ObservedAt)
	})
	return items, nil
}

// SetReview records an explicit approval or rejection.
func (s *Store) SetReview(id, status string) (*Observation, error) {
	if status != ReviewApproved && status != ReviewRejected {
		return nil, fmt.Errorf("unsupported review status %q", status)
	}
	o, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	now := nowUTC()
	o.Review.Status = status
	o.Review.ReviewedAt = &now
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal observation: %w", err)
	}
	path := filepath.Join(s.root, id+".json")
	tmp, err := os.CreateTemp(s.root, ".review-*.json")
	if err != nil {
		return nil, fmt.Errorf("create review update: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, fmt.Errorf("replace observation: %w", err)
	}
	return o, nil
}

var nowUTC = func() time.Time { return time.Now().UTC() }
