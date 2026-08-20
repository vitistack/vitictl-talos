package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl-talos/internal/release"
)

func newVersionCmd() *cobra.Command {
	var check bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the viti-talos version",
		Long: `Print the installed viti-talos version.

With --check, also ask GitHub for the latest published release and report
whether this build is current. Being offline is not a failure of "version",
so a check that cannot complete says so and exits zero.

This is the plugin's own version. For the Talos version a cluster runs, see
"viti talos nodes <cluster>", and for what the nodes actually booted rather
than what their images declare, "viti talos dashboard <cluster>".`,
		Example: `  viti talos version
  viti talos version --check`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			// Same wording as the root command's version template, so
			// "viti talos version" and "viti talos --version" never disagree.
			if _, err := fmt.Fprintf(out, "viti-talos version %s\n", version); err != nil {
				return err
			}
			if !check {
				return nil
			}
			return printReleaseCheck(cmd.Context(), out, version)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false,
		"check GitHub for a newer release and print upgrade instructions")
	return cmd
}

// printReleaseCheck reports whether the local build is up to date.
//
// A network or authentication failure is printed but not returned:
// `version --check` must never exit non-zero merely because the user is
// offline. Anything that stops the check is reported on the same stream as the
// version itself, so it is never silent.
func printReleaseCheck(ctx context.Context, out io.Writer, local string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	latest, err := release.FetchLatest(ctx, release.Repo)
	if err != nil {
		_, err := fmt.Fprintf(out, "⚠️  could not check for updates: %v\n", err)
		return err
	}
	return printReleaseStatus(out, local, latest)
}

// printReleaseStatus renders the comparison between the local build and the
// latest release. It is split from the fetch so every branch stays testable
// without reaching the network.
func printReleaseStatus(out io.Writer, local string, latest *release.Latest) error {
	switch release.Compare(local, latest.Tag) {
	case release.StatusUpToDate:
		_, _ = fmt.Fprintf(out, "✅ you are on the latest release (%s)\n", latest.Tag)
	case release.StatusOutdated:
		_, _ = fmt.Fprintf(out, "🆕 a newer release is available: %s (you have %s)\n", latest.Tag, local)
		_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
		_, _ = fmt.Fprintf(out, "   upgrade with:  %s\n", release.UpgradeHint())
		_, _ = fmt.Fprintln(out, "   or run:        viti talos upgrade --run")
	case release.StatusAhead:
		_, _ = fmt.Fprintf(out, "🧪 your build (%s) is ahead of the latest release (%s)\n", local, latest.Tag)
	case release.StatusDevelopment:
		_, _ = fmt.Fprintf(out, "🛠  development build (%s); latest release is %s\n", local, latest.Tag)
		_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
	}
	return nil
}
