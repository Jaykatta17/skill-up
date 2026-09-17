package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/alibaba/skill-up/internal/observation"
)

var (
	observeDataDir   string
	observeListJSON  bool
	observeCaseWrite bool
	observeSkillRoot string
)

var observeCmd = &cobra.Command{
	Use:   "observe",
	Short: "Review local Skill observations and create candidate cases",
}

var observeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List locally stored observations",
	Args:  usageOnError(cobra.NoArgs),
	RunE: func(cmd *cobra.Command, _ []string) error {
		store, err := observationStore()
		if err != nil {
			return err
		}
		items, err := store.List()
		if err != nil {
			return err
		}
		if observeListJSON {
			return writeJSON(cmd.OutOrStdout(), items)
		}
		for _, item := range items {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), item.Summary()); err != nil {
				return err
			}
		}
		return nil
	},
}

var observeShowCmd = &cobra.Command{
	Use:   "show <observation-id>",
	Short: "Show one observation",
	Args:  usageOnError(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := observationStore()
		if err != nil {
			return err
		}
		item, err := store.Load(args[0])
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), item)
	},
}

func reviewCommand(status string) *cobra.Command {
	return &cobra.Command{
		Use:   status + " <observation-id>",
		Short: status + " an observation for case conversion",
		Args:  usageOnError(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := observationStore()
			if err != nil {
				return err
			}
			item, err := store.SetReview(args[0], status)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s\n", item.ID, item.Review.Status)
			return err
		},
	}
}

var observeCaseCmd = &cobra.Command{
	Use:   "case <observation-id>",
	Short: "Preview or write a candidate regression case",
	Args:  usageOnError(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := observationStore()
		if err != nil {
			return err
		}
		item, err := store.Load(args[0])
		if err != nil {
			return err
		}
		if !observeCaseWrite {
			data, _, err := observation.CandidateCase(item)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		}
		if observeSkillRoot == "" {
			return errors.New("--skill-root is required with --write")
		}
		path, err := observation.WriteCandidateCase(item, observeSkillRoot)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s and updated %s\n", path, filepath.Join(observeSkillRoot, "evals", "eval.yaml"))
		return err
	},
}

var observeIngestHookCmd = &cobra.Command{
	Use:    "ingest-hook",
	Short:  "Ingest a Codex hook payload",
	Hidden: true,
	Args:   usageOnError(cobra.NoArgs),
	RunE: func(cmd *cobra.Command, _ []string) error {
		var input observation.HookInput
		decoder := json.NewDecoder(cmd.InOrStdin())
		if err := decoder.Decode(&input); err != nil {
			return fmt.Errorf("decode hook payload: %w", err)
		}
		store, err := observationStore()
		if err != nil {
			return err
		}
		if _, err := observation.NewHookHandler(store).Handle(input); err != nil {
			return err
		}
		// Stop and Interrupt hooks require JSON output on a successful exit;
		// an empty object is also valid for the other configured events.
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "{}")
		return err
	},
}

var observeMCPCmd = &cobra.Command{
	Use:    "mcp",
	Short:  "Run the observer marker MCP server",
	Hidden: true,
	Args:   usageOnError(cobra.NoArgs),
	RunE: func(cmd *cobra.Command, _ []string) error {
		return observation.ServeMCP(cmd.InOrStdin(), cmd.OutOrStdout())
	},
}

func observationStore() (*observation.Store, error) {
	root := observeDataDir
	if root == "" {
		var err error
		root, err = observation.DefaultDataDir()
		if err != nil {
			return nil, err
		}
	}
	return observation.NewStore(root), nil
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func init() {
	observeCmd.PersistentFlags().StringVar(&observeDataDir, "data-dir", "", "Observation directory (default: $SKILL_UP_OBSERVATION_DIR or ~/.skill-up/observations)")
	observeListCmd.Flags().BoolVar(&observeListJSON, "json", false, "Print JSON")
	observeCaseCmd.Flags().BoolVar(&observeCaseWrite, "write", false, "Write the approved candidate into the Skill eval suite")
	observeCaseCmd.Flags().StringVar(&observeSkillRoot, "skill-root", "", "Skill directory containing SKILL.md and evals/eval.yaml")
	observeCmd.AddCommand(observeListCmd)
	observeCmd.AddCommand(observeShowCmd)
	observeCmd.AddCommand(reviewCommand(observation.ReviewApproved))
	observeCmd.AddCommand(reviewCommand(observation.ReviewRejected))
	observeCmd.AddCommand(observeCaseCmd)
	observeCmd.AddCommand(observeIngestHookCmd)
	observeCmd.AddCommand(observeMCPCmd)
}
