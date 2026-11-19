//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestServerBinaryStarts(t *testing.T) {
	binary := os.Getenv("RAGFLOW_TEST_SERVER_BINARY")
	if binary == "" {
		t.Skip("requires the freshly built server binary")
	}
	for _, mode := range []string{"api", "ingestor", "syncer"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, binary, "--"+mode, "--help").CombinedOutput()
			// The CLI exits 1 after printing help.
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "Usage: "+binary+" --"+mode+" [OPTIONS]") {
				t.Fatalf("binary did not reach its help handler: %v\n%s", err, output)
			}
		})
	}
}
