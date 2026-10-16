package btrfs

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// HelperCommand is the hidden site-deploy command that runs Helper. The daemon
// runs it inside a container, with the volumes directory mounted at HelperRoot.
const HelperCommand = "btrfs-helper"

// HelperRoot is where the helper container mounts the volumes directory.
const HelperRoot = "/volumes"

// Helper runs one operation: create PATH, snapshot SOURCE TARGET, or delete
// PATH. Every path must be inside HelperRoot, so a mistake in the caller can't
// reach anything else the container might see.
func Helper(args []string, stderr io.Writer) int {
	if err := helper(args); err != nil {
		fmt.Fprintf(stderr, "site-deploy %s: %v\n", HelperCommand, err)
		return 1
	}
	return 0
}

func helper(args []string) error {
	if len(args) == 0 {
		return errors.New("expected create, snapshot or delete")
	}
	for _, path := range args[1:] {
		if !strings.HasPrefix(path, HelperRoot+"/") {
			return fmt.Errorf("path %q is outside %s", path, HelperRoot)
		}
	}
	switch {
	case args[0] == "create" && len(args) == 2:
		return Create(args[1])
	case args[0] == "snapshot" && len(args) == 3:
		return Snapshot(args[1], args[2])
	case args[0] == "delete" && len(args) == 2:
		return Delete(args[1])
	}
	return fmt.Errorf("usage: %s create PATH | snapshot SOURCE TARGET | delete PATH", HelperCommand)
}
