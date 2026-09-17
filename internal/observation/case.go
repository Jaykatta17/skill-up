package observation

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/alibaba/skill-up/internal/config"
)

var nonCaseIDChar = regexp.MustCompile(`[^a-z0-9-]+`)

type candidateCase struct {
	ID          string         `yaml:"id"`
	Title       string         `yaml:"title"`
	Description string         `yaml:"description"`
	Tag         string         `yaml:"tag"`
	Input       candidateInput `yaml:"input"`
}

type candidateInput struct {
	Prompt string `yaml:"prompt"`
}

// CandidateCase renders an observation as a valid case that inherits the
// suite-level judge. The generated description keeps provenance while making
// clear that expected behavior still needs human refinement.
func CandidateCase(o *Observation) ([]byte, string, error) {
	if err := o.Validate(); err != nil {
		return nil, "", err
	}
	base := strings.ToLower(strings.TrimSpace(o.Skill.Name))
	base = strings.ReplaceAll(base, "_", "-")
	base = nonCaseIDChar.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "skill"
	}
	suffix := strings.TrimPrefix(o.ID, "obs_")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	id := "observed-" + base + "-" + suffix
	description := "Candidate regression case from local observation " + o.ID + ". Review the prompt and add concrete expectations before relying on it as a release gate."
	if o.Feedback != nil && o.Feedback.Comment != "" {
		description += " User feedback: " + o.Feedback.Comment
	}
	c := candidateCase{
		ID:          id,
		Title:       "Observed " + o.Skill.Name + " interaction",
		Description: description,
		Tag:         "functional_test",
		Input:       candidateInput{Prompt: o.Input.Text},
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, "", fmt.Errorf("marshal candidate case: %w", err)
	}
	var parsed config.CaseConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, "", err
	}
	if err := config.NewValidator().ValidateCaseConfig(&parsed); err != nil {
		return nil, "", fmt.Errorf("generated case is invalid: %w", err)
	}
	return data, id, nil
}

// WriteCandidateCase writes an approved observation into a Skill's eval suite,
// updates cases.files, then validates the complete suite. It never overwrites
// an existing case and rolls back both changes if validation fails.
func WriteCandidateCase(o *Observation, skillRoot string) (string, error) {
	if o.Review.Status != ReviewApproved {
		return "", errors.New("observation must be approved before writing a case")
	}
	root, evalPath, evalOriginal, evalMode, err := prepareCaseWrite(skillRoot)
	if err != nil {
		return "", err
	}
	caseData, caseID, err := CandidateCase(o)
	if err != nil {
		return "", err
	}
	relCasePath := filepath.ToSlash(filepath.Join("evals", "cases", caseID+".yaml"))
	casePath := filepath.Join(root, filepath.FromSlash(relCasePath))
	wroteCase := false
	defer func() {
		if !wroteCase {
			_ = os.Remove(casePath)
		}
	}()
	if err := createCaseFile(casePath, caseData); err != nil {
		return "", err
	}

	evalUpdated, err := appendCaseFile(evalOriginal, relCasePath)
	if err != nil {
		return "", err
	}
	if err := replaceFile(evalPath, evalUpdated, evalMode); err != nil {
		return "", err
	}
	result, err := config.NewLoader(evalPath).LoadAll()
	if err != nil {
		return "", rollbackCandidate(evalPath, evalOriginal, evalMode, casePath, fmt.Errorf("load updated eval suite: %w", err))
	}
	if err := config.NewValidator().ValidateAll(result); err != nil {
		return "", rollbackCandidate(evalPath, evalOriginal, evalMode, casePath, fmt.Errorf("validate updated eval suite: %w", err))
	}
	wroteCase = true
	return casePath, nil
}

func rollbackCandidate(evalPath string, evalData []byte, evalMode os.FileMode, casePath string, cause error) error {
	errs := []error{cause}
	if err := replaceFile(evalPath, evalData, evalMode); err != nil {
		errs = append(errs, fmt.Errorf("restore eval.yaml: %w", err))
	}
	if err := os.Remove(casePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove candidate case: %w", err))
	}
	return errors.Join(errs...)
}

func prepareCaseWrite(skillRoot string) (root, evalPath string, evalData []byte, evalMode os.FileMode, err error) {
	root, err = filepath.Abs(skillRoot)
	if err != nil {
		return "", "", nil, 0, err
	}
	if _, err = os.Stat(filepath.Join(root, "SKILL.md")); err != nil {
		return "", "", nil, 0, fmt.Errorf("skill root must contain SKILL.md: %w", err)
	}
	evalPath = filepath.Join(root, "evals", "eval.yaml")
	evalData, err = os.ReadFile(evalPath)
	if err != nil {
		return "", "", nil, 0, fmt.Errorf("read eval.yaml: %w", err)
	}
	info, err := os.Stat(evalPath)
	if err != nil {
		return "", "", nil, 0, err
	}
	return root, evalPath, evalData, info.Mode().Perm(), nil
}

func createCaseFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("candidate case already exists: %s", path)
	}
	if err != nil {
		return fmt.Errorf("create candidate case: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func appendCaseFile(data []byte, relPath string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse eval.yaml: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("eval.yaml must be a mapping")
	}
	cases := mappingValue(doc.Content[0], "cases")
	if cases == nil || cases.Kind != yaml.MappingNode {
		return nil, errors.New("eval.yaml cases must be a mapping")
	}
	files := mappingValue(cases, "files")
	if files == nil || files.Kind != yaml.SequenceNode {
		return nil, errors.New("eval.yaml cases.files must be a sequence")
	}
	for _, item := range files.Content {
		if filepath.Clean(item.Value) == filepath.Clean(relPath) {
			return nil, fmt.Errorf("eval.yaml already references %s", relPath)
		}
	}
	files.Content = append(files.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: relPath})
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func replaceFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".skill-up-observe-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
