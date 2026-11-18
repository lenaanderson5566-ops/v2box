package cmd

import (
	"io"
	"log"
	"os"

	"github.com/lenaanderson5566-ops/v2box/node"
	"github.com/spf13/cobra"
)

func init() {
	command.AddCommand(&cobra.Command{
		Use: "internal-acme", Hidden: true, Args: cobra.NoArgs,
		SilenceErrors: true, SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			log.SetOutput(io.Discard)
			return node.RunACMEWorker(os.Stdin, os.Stdout)
		},
	})
}
