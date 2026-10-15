//go:build webssh

package main

import (
	"context"
	"errors"
	"os"

	"github.com/jpillora/sshd-lite/sshd"
)

// startWebSSH starts the browser terminal HTTP server when built with
// -tags webssh. It runs until ctx is cancelled.
func startWebSSH(ctx context.Context, token, port string, base sshd.Config) error {
	if token == "" {
		token = os.Getenv("SSHD_WEBSSH_TOKEN")
	}
	_ = port
	return errors.New("webssh: not implemented yet: " + token)
}
