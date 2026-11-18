package applications

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"rightproperty365.com/backend/internal/platform/storage"
)

// RetentionInterval is how often the worker erases applications past their
// form's retention period and removes the objects of deleted files.
const RetentionInterval = time.Hour

// Purge removes the stored objects of deleted files (queued by the
// database writes) and returns how many it removed.
func (s *Service) Purge(ctx context.Context) (int, error) {
	if s.files == nil {
		return 0, nil
	}
	done := 0
	for {
		keys, err := s.store.PendingPurges(ctx, PurgeBatch)
		if err != nil || len(keys) == 0 {
			return done, err
		}
		var removed []string
		var failed error
		for _, k := range keys {
			if err := s.files.Delete(ctx, k); err != nil && !errors.Is(err, storage.ErrNotFound) {
				failed = err
				continue
			}
			removed = append(removed, k)
		}
		if err := s.store.DonePurges(ctx, removed); err != nil {
			return done, err
		}
		done += len(removed)
		if failed != nil {
			return done, failed
		}
		if len(keys) < PurgeBatch {
			return done, nil
		}
	}
}

// RunRetention erases the answers and files of every application whose
// final state (ACCEPTED, REJECTED, WITHDRAWN) is older than its form
// version's retention_days. The row stays as a tombstone (reference,
// status, dates, listing) with an ERASED timeline entry; each erasure is
// logged. Then the deleted files' objects are removed.
func (s *Service) RunRetention(ctx context.Context) (erased, purged int, err error) {
	now := s.clock()
	for {
		due, err := s.store.DueForErasure(ctx, now, RetentionBatch)
		if err != nil {
			return erased, purged, err
		}
		for _, a := range due {
			ok, err := s.store.Erase(ctx, a.ID, ErasedByRetention, now)
			if err != nil {
				return erased, purged, err
			}
			if ok {
				erased++
				s.audit(ctx, "application.erased", slog.String("application_id", a.ID.String()), slog.String("reference", a.Reference),
					slog.String("reason", ErasedByRetention), slog.String("status", a.Status))
			}
		}
		if len(due) < RetentionBatch {
			break
		}
	}
	purged, err = s.Purge(ctx)
	return erased, purged, err
}

// RunJobs runs RunRetention every RetentionInterval until ctx ends.
func (s *Service) RunJobs(ctx context.Context) {
	t := time.NewTicker(RetentionInterval)
	defer t.Stop()
	for {
		if erased, purged, err := s.RunRetention(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("application retention job failed", slog.String("error", err.Error()))
		} else if erased > 0 || purged > 0 {
			s.logger.Info("application retention job", slog.Int("erased", erased), slog.Int("files_removed", purged))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
