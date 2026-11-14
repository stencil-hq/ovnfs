//go:build !windows

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMountCommandDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := mountOutput(ctx, []string{"/bin/sh", "-c", `trap '' TERM; sleep 30 & wait`})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung mount command: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("deadline waited for the child: %s", elapsed)
	}
}

func TestMountCommandFailure(t *testing.T) {
	_, err := mountOutput(t.Context(), []string{"/bin/sh", "-c", "echo 'resource busy' >&2; exit 7"})
	if err == nil || !strings.Contains(err.Error(), "resource busy") || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("lost mount failure diagnostic: %v", err)
	}
}
