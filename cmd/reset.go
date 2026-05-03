package cmd

import (
	"fmt"

	"github.com/ksred/cctrack/internal/config"
	"github.com/ksred/cctrack/internal/parser"
	"github.com/ksred/cctrack/internal/store"
	"github.com/spf13/cobra"
)

var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Clear all parsed data and re-ingest logs from scratch",
	Long: `Clears the sessions, requests, and file_offsets tables, then
re-parses every JSONL log file from the beginning.

Useful when upgrading from an older binary whose schema didn't include
the requests table — historical hour-level cost data won't be visible
until the request rows are written, and the parser only writes new
appends by default.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}

		s, err := store.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("opening store: %w", err)
		}
		defer s.Close()

		if err := s.ResetParsedData(); err != nil {
			return fmt.Errorf("clearing data: %w", err)
		}
		fmt.Println("Cleared sessions, requests, and file_offsets.")

		p := parser.New(s)
		files, sessions, err := p.ParseAll(cfg.LogDir)
		if err != nil {
			return fmt.Errorf("re-parsing: %w", err)
		}
		fmt.Printf("Re-parsed %d files, %d sessions reconstructed.\n", files, sessions)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(resetCmd)
}
