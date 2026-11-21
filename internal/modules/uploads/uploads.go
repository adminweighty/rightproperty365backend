// Package uploads is upload intents (API 0.60.0, FR-010): a signed-in client
// asks to upload one file for a purpose, PUTs it straight to private storage
// (a short-lived presigned URL with S3, or the API itself with local
// storage), then completes it. Completing checks the size (declared and the
// purpose's limit), the SHA-256, the real type (sniffed, never the client's
// word) and malware; a file that fails is deleted at once and never served.
// A READY file is used once by its consumer (for example a listing photo,
// which re-encodes it and so strips EXIF and GPS). Intents not used within
// a day are deleted with their objects.
package uploads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Purposes.
const (
	PurposeListingPhoto = "LISTING_PHOTO"
)

// Statuses.
const (
	StatusPendingUpload = "PENDING_UPLOAD"
	StatusReady         = "READY"
	StatusRejected      = "REJECTED"
	StatusUsed          = "USED"
	StatusExpired       = "EXPIRED"
)

// Rejection reasons (stored and shown).
const (
	ReasonMissing      = "NOT_UPLOADED"
	ReasonSizeMismatch = "SIZE_MISMATCH"
	ReasonTooLarge     = "TOO_LARGE"
	ReasonChecksum     = "CHECKSUM_MISMATCH"
	ReasonType         = "TYPE_NOT_ALLOWED"
	ReasonInfected     = "INFECTED"
	ReasonTooLargeScan = "TOO_LARGE_TO_SCAN"
)

// Timing.
const (
	// UploadWindow is how long the upload URL works.
	UploadWindow = 10 * time.Minute
	// Lifetime is how long an intent may wait to be used before the sweep
	// deletes it and its object.
	Lifetime = 24 * time.Hour
	// MaxOpenPerUser caps the intents a person may have waiting.
	MaxOpenPerUser = 50
	// SweepInterval is how often the orphan sweep runs.
	SweepInterval = 15 * time.Minute
)

// Rule is what a purpose accepts.
type Rule struct {
	Types    []string
	MaxBytes int64
}

// Rules are the allowlists, per purpose. The listing photo limits are the
// multipart upload's (listings.MaxPhotoBytes).
func Rules() map[string]Rule {
	return map[string]Rule{
		PurposeListingPhoto: {Types: []string{"image/jpeg", "image/png", "image/webp"}, MaxBytes: 15 << 20},
	}
}

// Errors.
var (
	ErrNotFound = errors.New("upload intent not found")
	ErrState    = errors.New("the upload intent is not in a state for this")
	ErrExpired  = errors.New("the upload window has closed")
	ErrTooMany  = errors.New("too many uploads waiting")
	ErrNoUpload = errors.New("signed in required")
	ErrRejected = errors.New("the upload was rejected")
)

// ValidationError carries per-field messages.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// RejectedError says why Complete rejected a file.
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return "upload rejected: " + e.Reason }

// Intent is one upload intent.
type Intent struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Purpose     string
	ContentType string
	SizeBytes   int64
	SHA256      string
	ObjectKey   string
	Status      string
	Reason      string
	CreatedAt   time.Time
	UploadBy    time.Time
	CompletedAt *time.Time
	UsedAt      *time.Time
	ExpiresAt   time.Time
}

// Upload says where and how to PUT the file.
type Upload struct {
	Method    string
	URL       string
	Headers   map[string]string
	Direct    bool // true: straight to storage; false: through the API
	ExpiresAt time.Time
}

// Store persists the intents.
type Store interface {
	Create(ctx context.Context, i Intent) error
	Get(ctx context.Context, id uuid.UUID) (Intent, error)
	// OpenCount counts a user's intents waiting (PENDING_UPLOAD or READY).
	OpenCount(ctx context.Context, user uuid.UUID, now time.Time) (int, error)
	// Transition moves an intent from one of from to to (compare and set);
	// ok is false when it was in another status.
	Transition(ctx context.Context, id uuid.UUID, from []string, to, reason string, at time.Time) (ok bool, err error)
	// Stale lists intents past their expiry that are not USED or EXPIRED.
	Stale(ctx context.Context, now time.Time, limit int) ([]Intent, error)
}

// Deps wires the service.
type Deps struct {
	Store   Store
	Storage storage.Storage
	// APIBaseURL is the API's public origin, for the through-API upload URL.
	APIBaseURL string
	Logger     *slog.Logger
	Now        func() time.Time
}

// Service holds the rules.
type Service struct {
	store   Store
	files   storage.Storage
	apiBase string
	logger  *slog.Logger
	now     func() time.Time
}

// NewService builds the service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, files: d.Storage, apiBase: strings.TrimRight(d.APIBaseURL, "/"), logger: d.Logger, now: d.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// Input is the body of POST /v1/media/upload-intents.
type Input struct {
	Purpose     string `json:"purpose"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func callerOf(p *auth.Principal) (uuid.UUID, error) {
	if p == nil || p.UserID == uuid.Nil {
		return uuid.Nil, ErrNoUpload
	}
	return p.UserID, nil
}

// Create authorises one upload and returns where to PUT it.
func (s *Service) Create(ctx context.Context, p *auth.Principal, in Input) (Intent, Upload, error) {
	user, err := callerOf(p)
	if err != nil {
		return Intent{}, Upload{}, err
	}
	fields := map[string]string{}
	rule, known := Rules()[in.Purpose]
	ct := strings.ToLower(strings.TrimSpace(in.ContentType))
	sum := strings.ToLower(strings.TrimSpace(in.SHA256))
	switch {
	case !known:
		fields["purpose"] = "must be LISTING_PHOTO"
	default:
		if !slices.Contains(rule.Types, ct) {
			fields["content_type"] = "must be one of " + strings.Join(rule.Types, ", ")
		}
		if in.SizeBytes <= 0 || in.SizeBytes > rule.MaxBytes {
			fields["size_bytes"] = fmt.Sprintf("must be between 1 and %d bytes", rule.MaxBytes)
		}
	}
	if !shaRe.MatchString(sum) {
		fields["sha256"] = "must be the file's SHA-256 in hexadecimal (64 characters)"
	}
	if len(fields) > 0 {
		return Intent{}, Upload{}, &ValidationError{Fields: fields}
	}
	now := s.clock()
	if n, err := s.store.OpenCount(ctx, user, now); err != nil {
		return Intent{}, Upload{}, err
	} else if n >= MaxOpenPerUser {
		return Intent{}, Upload{}, ErrTooMany
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Intent{}, Upload{}, err
	}
	// A random key, unrelated to the intent's id.
	key, err := storage.NewKey("intents")
	if err != nil {
		return Intent{}, Upload{}, err
	}
	i := Intent{ID: id, UserID: user, Purpose: in.Purpose, ContentType: ct, SizeBytes: in.SizeBytes, SHA256: sum,
		ObjectKey: key, Status: StatusPendingUpload, CreatedAt: now, UploadBy: now.Add(UploadWindow), ExpiresAt: now.Add(Lifetime)}
	if err := s.store.Create(ctx, i); err != nil {
		return Intent{}, Upload{}, err
	}
	up := Upload{Method: http.MethodPut, Headers: map[string]string{"Content-Type": ct}, ExpiresAt: i.UploadBy}
	url, direct, err := storage.PresignPut(ctx, s.files, i.ObjectKey, UploadWindow)
	if err != nil {
		return Intent{}, Upload{}, fmt.Errorf("presign the upload: %w", err)
	}
	if direct {
		up.URL, up.Direct = url, true
	} else {
		up.URL = s.apiBase + "/v1/media/upload-intents/" + id.String() + "/content"
	}
	return i, up, nil
}

// own returns the caller's intent (another person's is ErrNotFound).
func (s *Service) own(ctx context.Context, p *auth.Principal, id uuid.UUID) (Intent, error) {
	user, err := callerOf(p)
	if err != nil {
		return Intent{}, err
	}
	i, err := s.store.Get(ctx, id)
	if err != nil {
		return Intent{}, err
	}
	if i.UserID != user {
		return Intent{}, ErrNotFound
	}
	return i, nil
}

// Get returns the caller's intent.
func (s *Service) Get(ctx context.Context, p *auth.Principal, id uuid.UUID) (Intent, error) {
	return s.own(ctx, p, id)
}

// PutContent stores the file through the API (local storage, or a client
// that cannot PUT to storage directly). At most the declared size is read.
func (s *Service) PutContent(ctx context.Context, p *auth.Principal, id uuid.UUID, body io.Reader) error {
	i, err := s.own(ctx, p, id)
	if err != nil {
		return err
	}
	if i.Status != StatusPendingUpload {
		return ErrState
	}
	if s.clock().After(i.UploadBy) {
		return ErrExpired
	}
	content, err := io.ReadAll(io.LimitReader(body, i.SizeBytes+1))
	if err != nil {
		return fmt.Errorf("read the upload: %w", err)
	}
	if int64(len(content)) != i.SizeBytes {
		return &ValidationError{Fields: map[string]string{"body": fmt.Sprintf("must be exactly %d bytes, as declared", i.SizeBytes)}}
	}
	return s.files.Put(ctx, i.ObjectKey, bytes.NewReader(content), int64(len(content)), i.ContentType)
}

// readObject reads an intent's object, at most limit+1 bytes. nil, nil when
// there is none.
func (s *Service) readObject(ctx context.Context, i Intent, limit int64) ([]byte, error) {
	rc, err := s.files.Open(ctx, i.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit+1))
}

func (s *Service) reject(ctx context.Context, i Intent, reason string) error {
	if err := s.files.Delete(context.WithoutCancel(ctx), i.ObjectKey); err != nil && !errors.Is(err, storage.ErrNotFound) {
		s.logger.WarnContext(ctx, "upload intent: could not delete a rejected object", slog.String("intent_id", i.ID.String()), slog.String("error", err.Error()))
	}
	if _, err := s.store.Transition(ctx, i.ID, []string{StatusPendingUpload}, StatusRejected, reason, s.clock()); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "upload intent rejected", slog.String("intent_id", i.ID.String()), slog.String("reason", reason))
	return &RejectedError{Reason: reason}
}

// Complete checks the uploaded file and makes it READY, or rejects it.
func (s *Service) Complete(ctx context.Context, p *auth.Principal, id uuid.UUID) (Intent, error) {
	i, err := s.own(ctx, p, id)
	if err != nil {
		return Intent{}, err
	}
	switch i.Status {
	case StatusReady:
		return i, nil // completing twice changes nothing
	case StatusPendingUpload:
	case StatusRejected:
		return i, &RejectedError{Reason: i.Reason}
	default:
		return i, ErrState
	}
	rule := Rules()[i.Purpose]
	content, err := s.readObject(ctx, i, rule.MaxBytes)
	if err != nil {
		return Intent{}, err
	}
	if content == nil {
		if s.clock().After(i.UploadBy) {
			return Intent{}, s.reject(ctx, i, ReasonMissing)
		}
		// Not there yet: the client may complete again after its PUT.
		return Intent{}, &RejectedError{Reason: ReasonMissing}
	}
	if reason := check(i, rule, content); reason != "" {
		return Intent{}, s.reject(ctx, i, reason)
	}
	switch err := storage.Screen(ctx, s.files, "upload-intent", content); {
	case errors.Is(err, storage.ErrUploadInfected):
		return Intent{}, s.reject(ctx, i, ReasonInfected)
	case errors.Is(err, storage.ErrTooLargeToScan):
		return Intent{}, s.reject(ctx, i, ReasonTooLargeScan)
	case err != nil:
		return Intent{}, err // the scanner is down: try again later, nothing changes
	}
	now := s.clock()
	ok, err := s.store.Transition(ctx, i.ID, []string{StatusPendingUpload}, StatusReady, "", now)
	if err != nil {
		return Intent{}, err
	}
	if !ok {
		return s.store.Get(ctx, i.ID)
	}
	i.Status, i.CompletedAt = StatusReady, &now
	return i, nil
}

// check returns why content does not match its intent ("" when it does).
func check(i Intent, rule Rule, content []byte) string {
	switch {
	case int64(len(content)) > rule.MaxBytes:
		return ReasonTooLarge
	case int64(len(content)) != i.SizeBytes:
		return ReasonSizeMismatch
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != i.SHA256 {
		return ReasonChecksum
	}
	sniffed := http.DetectContentType(content)
	if !slices.Contains(rule.Types, sniffed) || sniffed != i.ContentType {
		return ReasonType
	}
	return ""
}

// Read returns the content of the caller's READY intent for purpose, for
// its consumer. Consume marks it used once the consumer has succeeded.
func (s *Service) Read(ctx context.Context, p *auth.Principal, id uuid.UUID, purpose string) ([]byte, error) {
	i, err := s.own(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if i.Purpose != purpose || i.Status != StatusReady {
		return nil, ErrState
	}
	content, err := s.readObject(ctx, i, Rules()[purpose].MaxBytes)
	if err != nil {
		return nil, err
	}
	if content == nil {
		return nil, ErrState
	}
	// The object could have been replaced after Complete: check again.
	if reason := check(i, Rules()[purpose], content); reason != "" {
		return nil, s.reject(ctx, i, reason)
	}
	return content, nil
}

// Consume marks a READY intent USED and deletes its object.
func (s *Service) Consume(ctx context.Context, id uuid.UUID) error {
	i, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	ok, err := s.store.Transition(ctx, id, []string{StatusReady}, StatusUsed, "", s.clock())
	if err != nil {
		return err
	}
	if !ok {
		return ErrState
	}
	if err := s.files.Delete(context.WithoutCancel(ctx), i.ObjectKey); err != nil && !errors.Is(err, storage.ErrNotFound) {
		s.logger.WarnContext(ctx, "upload intent: could not delete a used object", slog.String("intent_id", id.String()), slog.String("error", err.Error()))
	}
	return nil
}

// Sweep deletes the intents (and objects) not used in time. It returns how
// many expired.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	now := s.clock()
	stale, err := s.store.Stale(ctx, now, 500)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, i := range stale {
		if err := s.files.Delete(ctx, i.ObjectKey); err != nil && !errors.Is(err, storage.ErrNotFound) {
			s.logger.WarnContext(ctx, "upload intent sweep: delete failed", slog.String("intent_id", i.ID.String()), slog.String("error", err.Error()))
			continue
		}
		ok, err := s.store.Transition(ctx, i.ID, []string{StatusPendingUpload, StatusReady, StatusRejected}, StatusExpired, i.Reason, now)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}
