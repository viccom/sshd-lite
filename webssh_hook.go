//go:build webssh

package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jpillora/sshd-lite/sshd"
	"github.com/jpillora/sshd-lite/webssh"
)

// startWebSSH serves the browser terminal UI and WebSocket until ctx is
// cancelled. Empty token falls back to SSHD_WEBSSH_TOKEN, then errors.
func startWebSSH(ctx context.Context, token, port, origins string, base sshd.Config) error {
	if token == "" {
		token = os.Getenv("SSHD_WEBSSH_TOKEN")
	}
	h, err := webssh.New(webssh.Options{
		Token:          token,
		OriginPatterns: splitOrigins(origins),
	}, base)
	if err != nil {
		return err
	}
	if port == "" {
		port = "8022"
	}
	l, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return errors.New("webssh: " + err.Error())
	}
	srv := &http.Server{Handler: h.Routes()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("webssh: %v", err)
		}
	}()
	log.Printf("webssh: listening on http://0.0.0.0:%s/ (token required)", port)
	return nil
}

// splitOrigins parses the comma-separated --webssh-origin value into glob
// patterns; empty input yields nil (same-origin only).
func splitOrigins(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
