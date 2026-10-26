package cmd

import (
	"encoding/json"
	"github.com/spf13/cobra"
)

func init() {
	command.AddCommand(&cobra.Command{Use: "capabilities", Args: cobra.NoArgs, Short: "Show deployment capabilities", RunE: func(cmd *cobra.Command, _ []string) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"features": []string{"multi-panel-v1", "reload-signal-v1", "shared-anytls-v1", "shared-listener-v1"}})
	}})
}
