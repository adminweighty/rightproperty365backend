// Package dataexport gives a person a copy of their data (API 0.55.0, FSD
// section 9 "data export/delete requests"): they ask for it, the worker
// builds a ZIP of JSON files (one per section) into the private bucket, they
// are told by email and in-app, and they download it through the API for 7
// days, after which the object is deleted. Only data about the person as an
// account holder and customer is included, and never another person's
// contact details.
package dataexport

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/storage"
)

// Statuses.
const (
	StatusRequested = "REQUESTED"
	StatusBuilding  = "BUILDING"
	StatusReady     = "READY"
	StatusFailed    = "FAILED"
	StatusExpired   = "EXPIRED"
)

// Timing and limits.
const (
	Lifetime      = 7 * 24 * time.Hour
	Cooldown      = 24 * time.Hour
	SweepInterval = time.Minute
	buildLease    = 10 * time.Minute
	maxAttempts   = 3
	sweepBatch    = 5
)

// Outbox event.
const (
	AggregateExport = "data_export"
	EventReady      = "DATA_EXPORT_READY"
)

// EventTypes lists the events this module writes.
func EventTypes() []string { return []string{EventReady} }

// Errors.
var (
	ErrRecent   = errors.New("an export was requested in the last 24 hours")
	ErrNotReady = errors.New("no export is ready to download")
)

// Export is one export request.
type Export struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	Status         string
	ObjectKey      string
	SizeBytes      int64
	Attempts       int
	RequestedAt    time.Time
	ReadyAt        *time.Time
	ExpiresAt      *time.Time
	Downloads      int
	LastDownloadAt *time.Time
	ExpiredAt      *time.Time
}

// Store persists export requests.
type Store interface {
	// Latest returns the user's newest request (ok=false for none).
	Latest(ctx context.Context, userID uuid.UUID) (Export, bool, error)
	// Create stores a REQUESTED export unless the user has one requested
	// since `since` that did not fail; then that one is returned with
	// created=false. Checked under a per-user lock.
	Create(ctx context.Context, e Export, since time.Time) (Export, bool, error)
	// Claim moves up to limit REQUESTED (or BUILDING past their lease)
	// exports to BUILDING, counting the attempt, and returns them.
	Claim(ctx context.Context, now, leaseUntil time.Time, limit int) ([]Export, error)
	// Ready marks a built export READY and writes EventReady in the same
	// transaction.
	Ready(ctx context.Context, id uuid.UUID, key string, size int64, now, expires time.Time) error
	// Fail puts the export back to REQUESTED, or FAILED after maxAttempts.
	Fail(ctx context.Context, id uuid.UUID, final bool) error
	// Expire returns the READY exports past their expiry and marks them
	// EXPIRED (the caller deletes the objects first).
	DueForExpiry(ctx context.Context, now time.Time, limit int) ([]Export, error)
	MarkExpired(ctx context.Context, id uuid.UUID, now time.Time) error
	// Downloaded counts a download.
	Downloaded(ctx context.Context, id uuid.UUID, now time.Time) error
}

// Section is one JSON file of the archive.
type Section struct {
	Name string
	Rows any
}

// Collector gathers a person's data, section by section.
type Collector interface {
	Collect(ctx context.Context, userID uuid.UUID) ([]Section, error)
}

// Service holds the rules.
type Service struct {
	store     Store
	files     storage.Storage
	collector Collector
	logger    *slog.Logger
	now       func() time.Time
}

// NewService builds the service. collector is needed by the worker only.
func NewService(store Store, files storage.Storage, collector Collector, logger *slog.Logger, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, files: files, collector: collector, logger: logger, now: now}
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// Request asks for an export (ErrRecent with the existing one within 24 hours).
func (s *Service) Request(ctx context.Context, userID uuid.UUID) (Export, error) {
	now := s.clock()
	id, err := uuid.NewV7()
	if err != nil {
		return Export{}, err
	}
	e, created, err := s.store.Create(ctx, Export{ID: id, UserID: userID, Status: StatusRequested, RequestedAt: now}, now.Add(-Cooldown))
	if err != nil {
		return Export{}, err
	}
	if !created {
		return e, ErrRecent
	}
	s.logger.InfoContext(ctx, "data export requested", slog.String("export_id", e.ID.String()))
	return e, nil
}

// Latest returns the user's newest export (nil for none).
func (s *Service) Latest(ctx context.Context, userID uuid.UUID) (*Export, error) {
	e, ok, err := s.store.Latest(ctx, userID)
	if err != nil || !ok {
		return nil, err
	}
	return &e, nil
}

// Open returns the user's READY archive.
func (s *Service) Open(ctx context.Context, userID uuid.UUID) (Export, io.ReadCloser, error) {
	e, ok, err := s.store.Latest(ctx, userID)
	if err != nil {
		return Export{}, nil, err
	}
	now := s.clock()
	if !ok || e.Status != StatusReady || e.ExpiresAt == nil || !e.ExpiresAt.After(now) {
		return Export{}, nil, ErrNotReady
	}
	rc, err := s.files.Open(ctx, e.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		return Export{}, nil, ErrNotReady
	}
	if err != nil {
		return Export{}, nil, err
	}
	if err := s.store.Downloaded(ctx, e.ID, now); err != nil {
		_ = rc.Close()
		return Export{}, nil, err
	}
	s.logger.InfoContext(ctx, "data export downloaded", slog.String("export_id", e.ID.String()))
	return e, rc, nil
}

// readme is the archive's first file.
const readme = `Your RightProperty365 data
==========================

This archive holds the data RightProperty365 keeps about you as an account
holder and customer, one JSON file per kind of data. Times are in UTC.

Other people appear only by an identifier or their public name: their contact
details are not included. Data of an organisation you work for (its contact
book, ledgers, tenancies and the requests it received) belongs to that
organisation and is not part of this archive; ask the organisation for it.

To correct your data, change it in your account or contact support. To delete
your account, use "Delete your account" on your account page.
`

// Build writes the archive of a user.
func (s *Service) Build(ctx context.Context, userID uuid.UUID, at time.Time) ([]byte, error) {
	sections, err := s.collector.Collect(ctx, userID)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, body []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: at})
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	}
	if err := write("README.txt", []byte(readme+"\nCreated: "+at.UTC().Format(time.RFC3339)+"\n")); err != nil {
		return nil, err
	}
	for _, sec := range sections {
		body, err := json.MarshalIndent(sec.Rows, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", sec.Name, err)
		}
		if err := write(sec.Name+".json", body); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Sweep builds the requested exports and expires the old ones. It returns
// how many it built.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	now := s.clock()
	var errs []error
	due, err := s.store.DueForExpiry(ctx, now, 50)
	if err != nil {
		errs = append(errs, err)
	}
	for _, e := range due {
		if err := s.files.Delete(ctx, e.ObjectKey); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.store.MarkExpired(ctx, e.ID, now); err != nil {
			errs = append(errs, err)
		}
	}
	claimed, err := s.store.Claim(ctx, now, now.Add(buildLease), sweepBatch)
	if err != nil {
		return 0, errors.Join(append(errs, err)...)
	}
	built := 0
	for _, e := range claimed {
		if err := s.buildOne(ctx, e, now); err != nil {
			s.logger.ErrorContext(ctx, "data export build failed", slog.String("export_id", e.ID.String()), slog.Int("attempt", e.Attempts), slog.String("error", err.Error()))
			if ferr := s.store.Fail(ctx, e.ID, e.Attempts >= maxAttempts); ferr != nil {
				errs = append(errs, ferr)
			}
			continue
		}
		built++
	}
	return built, errors.Join(errs...)
}

func (s *Service) buildOne(ctx context.Context, e Export, now time.Time) error {
	body, err := s.Build(ctx, e.UserID, now)
	if err != nil {
		return err
	}
	key, err := storage.NewKey("exports")
	if err != nil {
		return err
	}
	if err := s.files.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "application/zip"); err != nil {
		return err
	}
	if err := s.store.Ready(ctx, e.ID, key, int64(len(body)), now, now.Add(Lifetime)); err != nil {
		_ = s.files.Delete(ctx, key)
		return err
	}
	s.logger.InfoContext(ctx, "data export ready", slog.String("export_id", e.ID.String()), slog.Int("size", len(body)))
	return nil
}
