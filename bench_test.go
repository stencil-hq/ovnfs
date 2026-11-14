package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMeasureBenchPreservesFile(t *testing.T) {
	data := make([]byte, 3*4096)
	for i := range data {
		data[i] = byte(i*37 + i/4096)
	}
	for _, random := range []bool{false, true} {
		name := "sequential"
		if random {
			name = "random"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "io.bin")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			buffer := make([]byte, 4096)
			for _, write := range []bool{true, false} {
				m, err := measureBench(t.Context(), f, benchWorkload{block: 4096, write: write, random: random}, data, buffer, 10*time.Millisecond)
				if err != nil {
					t.Fatal(err)
				}
				if m.Operations == 0 || m.Bytes != m.Operations*4096 {
					t.Fatalf("invalid completed I/O accounting: %+v", m)
				}
				if m.MiBPS != float64(m.Bytes)/(1<<20)/m.Seconds || m.IOPS != float64(m.Operations)/m.Seconds {
					t.Fatalf("rates do not match completed work: %+v", m)
				}
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatal("benchmark changed seeded data or extended the working set")
			}
		})
	}
}

func TestMeasureBenchErrors(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "short-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, buffer := make([]byte, 4096), make([]byte, 4096)
	work := benchWorkload{block: 4096}
	if _, err := measureBench(t.Context(), f, work, data, buffer, time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("empty read: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := measureBench(ctx, f, work, data, buffer, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
}
