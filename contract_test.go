package watcher_test

import (
	"context"
	"errors"
	"testing"

	watcher "github.com/alexgorbatchev/agent-watcher"
)

func TestRunRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		cfg     watcher.Config
		handler watcher.Handler
	}{
		{name: "missing handler"},
		{name: "unknown harness", cfg: watcher.Config{Harnesses: []watcher.Harness{"unknown"}}, handler: func(context.Context, watcher.Event, func()) error { return nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := watcher.Run(context.Background(), tt.cfg, tt.handler); err == nil {
				t.Fatal("Run accepted invalid configuration")
			}
		})
	}
}

func TestRunCancelledContextDoesNotCreateState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := watcher.Run(ctx, watcher.Config{}, func(context.Context, watcher.Event, func()) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
}
