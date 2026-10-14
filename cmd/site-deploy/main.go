package main

import (
	"os"
	"site-deploy/internal/cli"
	"site-deploy/internal/daemon"
)

func main() {
	cli.Serve = daemon.Run
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
