package main

import (
	"os"
	"site-deploy/internal/btrfs"
	"site-deploy/internal/cli"
	"site-deploy/internal/daemon"
)

func main() {
	// The daemon runs this inside a helper container for site-deploy volumes.
	if len(os.Args) > 1 && os.Args[1] == btrfs.HelperCommand {
		os.Exit(btrfs.Helper(os.Args[2:], os.Stderr))
	}
	cli.Serve = daemon.Run
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
