package watcher_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	watcher "github.com/alexgorbatchev/agent-watcher"
)

func ExampleRun() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := watcher.Run(ctx, watcher.Config{}, func(ctx context.Context, event watcher.Event, acknowledge func()) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := fmt.Printf("%s %s %s\n", event.Harness, event.SessionID, event.EventType); err != nil {
			return err
		}
		if acknowledge != nil {
			acknowledge()
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
	}
}
