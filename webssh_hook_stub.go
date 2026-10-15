//go:build !webssh

package main

import (
	"context"
	"errors"

	"github.com/jpillora/sshd-lite/sshd"
)

// startWebSSH errors out: the binary was built without the webssh tag.
func startWebSSH(ctx context.Context, token, port, origins string, base sshd.Config) error {
	return errors.New("--webssh requires a build with -tags webssh")
}
