// Command ecsd deploys to ECS: register from templates, run the release task, roll every service, prove the new
// revision is what serves, record a release, roll back to one.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

var version = "" // set by release builds

// buildVersion is the release's version, else the module version go install recorded.
func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return "dev"
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := root()
	if err := cmd.ExecuteContext(ctx); err != nil {
		var exit exitError
		if errors.As(err, &exit) {
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// exitError ends the process with a code of its own, quietly: a one-off task's exit code.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func root() *cobra.Command { return rootWith(&app{}) }

func rootWith(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "ecsd",
		Short:         "Deploy to ECS: register, release, roll, verify, record, roll back",
		Version:       buildVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	f := cmd.PersistentFlags()
	f.StringVarP(&a.stage, "stage", "s", os.Getenv("ECSD_STAGE"), "stage to act on (ECSD_STAGE)")
	f.StringVarP(&a.configPath, "config", "c", envOr("ECSD_CONFIG", "ecsd.yml"), "ecsd.yml path (ECSD_CONFIG)")
	f.StringVar(&a.manifestRef, "manifest", os.Getenv("ECSD_MANIFEST"), "ssm:/path or file:path (ECSD_MANIFEST; default ssm:/<app>/<stage>/deploy)")
	f.StringVar(&a.region, "region", "", "AWS region (default: the AWS config's)")
	f.StringVar(&a.repo, "repo", ".", "git repository the commits are in")

	cmd.AddCommand(
		deployCmd(a), planCmd(a), rollbackCmd(a), supersededCmd(a), historyCmd(a), statusCmd(a),
		runCmd(a), consoleCmd(a), execCmd(a), tunnelCmd(a), logsCmd(a), insightsCmd(a),
		manifestCmd(a), iamPolicyCmd(a), schemaCmd(), checkCmd(a),
	)
	return cmd
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
