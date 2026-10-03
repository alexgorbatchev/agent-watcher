package watcher

import (
	"context"
	"time"
)

func (r *runner) prune() {
	if count := r.cursors.Prune(r.cfg.CursorRetention); count > 0 {
		r.logger.Info("cursor_store_pruned", "pruned", count)
		if err := r.cursors.Save(); err != nil {
			r.logger.Warn("cursor_store_save_error", "err", err)
		}
	}
}

func (r *runner) persistPeriodically(ctx context.Context) {
	pruner := time.NewTicker(r.cfg.CursorPruneInterval)
	defer pruner.Stop()
	flusher := time.NewTicker(r.cfg.CursorFlushInterval)
	defer flusher.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pruner.C:
			r.prune()
		case <-flusher.C:
			if err := r.cursors.Flush(); err != nil {
				r.logger.Warn("cursor_store_flush_error", "err", err)
			}
			if err := r.checkpoints.flush(); err != nil {
				r.logger.Warn("database_checkpoint_flush_error", "err", err)
			}
		}
	}
}

func (r *runner) shutdown(cancel context.CancelFunc) {
	cancel()
	for _, active := range r.tails {
		active.cancel()
		active.tailer.Stop()
	}
	r.wg.Wait()
	if err := r.registry.Close(); err != nil {
		r.logger.Warn("watcher_registry_close_error", "err", err)
	}
	if err := r.cursors.Save(); err != nil {
		r.logger.Warn("cursor_store_save_error", "err", err)
	}
	if err := r.tracker.Save(); err != nil {
		r.logger.Warn("session_tracker_save_error", "err", err)
	}
	if err := r.checkpoints.flush(); err != nil {
		r.logger.Warn("database_checkpoint_flush_error", "err", err)
	}
}
