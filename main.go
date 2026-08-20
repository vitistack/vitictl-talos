// viti-talos adds Talos Linux commands to the viti CLI. vitictl discovers any
// viti-* binary on PATH as a subcommand, so this binary is reachable as
// "viti talos ..." — and as "viti t ..." through a viti-t link, created by
// "viti plugin install" from the aliases declared in viti's plugin index, or
// by this repo's "make install" for a source build.
package main

import (
	"os"

	"github.com/vitistack/vitictl-talos/cmd"
)

// Injected via -ldflags at build time; see the Makefile.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	cmd.SetVersion(version)
	_ = commit

	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
