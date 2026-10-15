// Package deploy holds the files that install site-deploy on a server. The
// CLI's upgrade command sends them along with the server's binary.
package deploy

import _ "embed"

// InstallHost installs or upgrades the daemon; it runs as root on the server.
//
//go:embed install-host.sh
var InstallHost []byte

// Service is the systemd unit that install-host.sh installs.
//
//go:embed site-deploy.service
var Service []byte
