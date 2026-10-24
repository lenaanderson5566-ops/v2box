package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"os"
	"os/exec"
	"runtime"
)

const managerPath = "/usr/local/lib/v2box/manager.sh"

func runManager(path string, args []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("service management is available on Linux only")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("management script is missing; reinstall program files using the panel installation command (configuration and certificates are retained)")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	var prefix [2]byte
	_, err = f.Read(prefix[:])
	f.Close()
	if err != nil || string(prefix[:]) != "#!" {
		return fmt.Errorf("invalid management script; reinstall program files")
	}
	child := exec.Command("/bin/bash", append([]string{path}, args...)...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	return child.Run()
}

func init() {
	for _, name := range []string{"start", "stop", "restart", "status", "enable", "disable", "log", "ip", "credential", "ddns", "update", "uninstall", "resume"} {
		command.AddCommand(&cobra.Command{Use: name, Short: "Manage installed V2Box: " + name, DisableFlagParsing: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runManager(managerPath, append([]string{cmd.Name()}, args...))
			},
		})
	}
}
