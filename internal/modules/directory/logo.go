package directory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registers the JPEG header decoder
	_ "image/png"  // registers the PNG header decoder
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"time"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/storage"
)

// A logo is the ONE file of a profile that is public. It is still kept in
// the private object storage and only ever leaves through
// GET /v1/directory/{kind}/{slug}/logo, which serves it for a profile that is
// public right now. Nothing here produces a storage URL.

// Logo limits. A logo is not resized, cropped or re-encoded: what was
// uploaded is what is served, so the limits are what keeps it reasonable.
const (
	MaxLogoBytes     = 2 << 20 // 2 MB
	MinLogoPixels    = 128
	MaxLogoPixels    = 4096
	LogoKeyPrefix    = "logos"
	LogoCacheMaxAge  = 5 * time.Minute
	LogoCacheStale   = 24 * time.Hour
	logoContentPNG   = "image/png"
	logoContentJPEG  = "image/jpeg"
	maxLogoBodyExtra = 64 << 10
)

// MaxLogoBodyBytes is the request body limit of a logo upload: the image
// plus room for the multipart framing.
const MaxLogoBodyBytes = MaxLogoBytes + maxLogoBodyExtra

// Error codes of the logo routes.
const (
	CodeLogoNotFound    = "LOGO_NOT_FOUND"
	CodeLogoTooLarge    = "FILE_TOO_LARGE"
	CodeLogoUnsupported = "UNSUPPORTED_FILE_TYPE"
)

// Errors of the logo routes.
var (
	ErrLogoNotFound    = errors.New("the profile has no logo")
	ErrLogoTooLarge    = errors.New("logo larger than 2 MB")
	ErrLogoUnsupported = errors.New("a logo must be a PNG or JPEG image")
	// ErrLogoDimensions is a logo smaller than 128x128 or larger than 4096x4096 pixels.
	ErrLogoDimensions = errors.New("a logo must be between 128x128 and 4096x4096 pixels")
	// ErrForbidden: the caller belongs to the profile's organisation but may
	// not manage the profile.
	ErrForbidden = errors.New("not allowed to manage this profile")
)

// Logo is a profile's logo as stored. ObjectKey is random and never leaves
// the API; Checksum is the SHA-256 of the image in hexadecimal (the ETag of
// the public route, and the source of the URL's v parameter).
type Logo struct {
	ObjectKey   string
	ContentType string
	Width       int
	Height      int
	Checksum    string
	Size        int64
	UpdatedAt   time.Time
}

// LogoImage is what CheckLogo learned about an uploaded image.
type LogoImage struct {
	ContentType string
	Width       int
	Height      int
	Checksum    string
}

// CheckLogo decides whether content may be a logo. The type is decided by
// the bytes alone (never a file name or a Content-Type header) and must be
// PNG or JPEG: SVG is refused because it can carry scripts, GIF, WebP and
// animated PNG because a logo is one still image. Only the image header is
// decoded, to learn the size in pixels.
func CheckLogo(content []byte) (LogoImage, error) {
	if len(content) > MaxLogoBytes {
		return LogoImage{}, ErrLogoTooLarge
	}
	contentType := http.DetectContentType(content)
	if contentType != logoContentPNG && contentType != logoContentJPEG {
		return LogoImage{}, ErrLogoUnsupported
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || (format == "png") != (contentType == logoContentPNG) || (format == "jpeg") != (contentType == logoContentJPEG) {
		return LogoImage{}, ErrLogoUnsupported
	}
	if format == "png" && animatedPNG(content) {
		return LogoImage{}, ErrLogoUnsupported
	}
	if cfg.Width < MinLogoPixels || cfg.Height < MinLogoPixels || cfg.Width > MaxLogoPixels || cfg.Height > MaxLogoPixels {
		return LogoImage{}, ErrLogoDimensions
	}
	sum := sha256.Sum256(content)
	return LogoImage{ContentType: contentType, Width: cfg.Width, Height: cfg.Height, Checksum: hex.EncodeToString(sum[:])}, nil
}

// animatedPNG reports whether a PNG has an animation control chunk (acTL)
// before its image data, which is what makes it an animated PNG.
func animatedPNG(content []byte) bool {
	const signature = 8
	for at := signature; at+8 <= len(content); {
		length := int(binary.BigEndian.Uint32(content[at : at+4]))
		switch string(content[at+4 : at+8]) {
		case "acTL":
			return true
		case "IDAT", "IEND":
			return false
		}
		if length < 0 || at+12+length > len(content) {
			return false
		}
		at += 12 + length // length, type, data, CRC
	}
	return false
}

// StoreLogo validates an image and stores it under a new random key. The
// caller attaches the returned Logo to a profile, and removes the object
// again if that fails.
func StoreLogo(ctx context.Context, files storage.Storage, content []byte, now time.Time) (Logo, error) {
	img, err := CheckLogo(content)
	if err != nil {
		return Logo{}, err
	}
	key, err := storage.NewKey(LogoKeyPrefix)
	if err != nil {
		return Logo{}, err
	}
	if err := files.Put(ctx, key, bytes.NewReader(content), int64(len(content)), img.ContentType); err != nil {
		return Logo{}, fmt.Errorf("store logo: %w", err)
	}
	return Logo{ObjectKey: key, ContentType: img.ContentType, Width: img.Width, Height: img.Height, Checksum: img.Checksum,
		Size: int64(len(content)), UpdatedAt: now.UTC()}, nil
}

// publicLogo is the public form of a record's logo (nil when it has none).
func publicLogo(r Record) *listings.Logo {
	if r.Logo == nil {
		return nil
	}
	return listings.NewLogo(r.Kind, r.Slug, r.Logo.Checksum, r.Logo.Width, r.Logo.Height)
}

// SetFiles gives the Service the object storage logos are kept in.
func (s *Service) SetFiles(files storage.Storage, logger *slog.Logger) {
	s.files, s.logger = files, logger
}

// SetClock overrides the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) clock() time.Time {
	if s.now == nil {
		return time.Now().UTC().Truncate(time.Microsecond)
	}
	return s.now().UTC().Truncate(time.Microsecond)
}

// LogoFile is a logo ready to be streamed.
type LogoFile struct {
	Logo    Logo
	Content io.ReadCloser
}

// PublicLogo returns the stored logo of a PUBLIC profile: ErrProfileNotFound
// when the profile is not public (unverified, suspended or unknown),
// ErrLogoNotFound when it has no logo.
func (s *Service) PublicLogo(ctx context.Context, kindCode, slug string) (Logo, error) {
	if _, err := s.kind(ctx, kindCode); err != nil {
		return Logo{}, err
	}
	r, err := s.store.Get(ctx, kindCode, slug) // published AND verified only
	if err != nil {
		return Logo{}, err
	}
	if r.Logo == nil {
		return Logo{}, ErrLogoNotFound
	}
	return *r.Logo, nil
}

// OpenLogo opens the image of a logo PublicLogo returned.
func (s *Service) OpenLogo(ctx context.Context, logo Logo) (io.ReadCloser, error) {
	if s.files == nil {
		return nil, ErrLogoNotFound
	}
	rc, err := s.files.Open(ctx, logo.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrLogoNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open logo: %w", err)
	}
	return rc, nil
}

// ManagedProfile is a profile as its manager sees it in GET /v1/me/profiles.
type ManagedProfile struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	// Verified is true when the profile is public (published and verified).
	Verified bool           `json:"verified"`
	Logo     *listings.Logo `json:"logo"`
}

func managed(r Record) ManagedProfile {
	return ManagedProfile{Kind: r.Kind, Slug: r.Slug, Name: r.Name, Verified: r.Verified && r.Status == StatusActive, Logo: publicLogo(r)}
}

// Mine returns the profiles the caller may manage: the business profiles of
// the organisations they own or manage, and their own agent profile.
func (s *Service) Mine(ctx context.Context, p *auth.Principal) ([]ManagedProfile, error) {
	records, err := s.store.Managed(ctx, p.OrganisationIDs(auth.RoleOwner, auth.RoleManager), p.UserID)
	if err != nil {
		return nil, err
	}
	out := []ManagedProfile{}
	for _, r := range records {
		if p.MayManageProfile(r.OrganisationID, r.AgentUserID) {
			out = append(out, managed(r))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// manageable loads a profile the caller may manage. A profile of an
// organisation the caller does not belong to is ErrProfileNotFound (its
// existence is not confirmed); one of their own organisation that their
// role may not manage is ErrForbidden.
func (s *Service) manageable(ctx context.Context, p *auth.Principal, kind, slug string) (Record, error) {
	r, err := s.store.Find(ctx, kind, slug)
	if err != nil {
		return Record{}, err
	}
	switch {
	case p.MayManageProfile(r.OrganisationID, r.AgentUserID):
		return r, nil
	case slices.Contains(p.OrganisationIDs(), r.OrganisationID):
		return Record{}, ErrForbidden
	}
	return Record{}, ErrProfileNotFound
}

func (s *Service) audit(ctx context.Context, event string, p *auth.Principal, r Record, attrs ...slog.Attr) {
	if s.logger == nil {
		return
	}
	base := []slog.Attr{slog.Bool("audit", true), slog.String("event", event), slog.String("trace_id", logging.RequestID(ctx)),
		slog.String("profile", r.Kind+"/"+r.Slug), slog.String("actor_user_id", p.UserID.String())}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", append(base, attrs...)...)
}

func (s *Service) removeObject(ctx context.Context, key string) {
	if key == "" || s.files == nil {
		return
	}
	if err := s.files.Delete(context.WithoutCancel(ctx), key); err != nil && s.logger != nil {
		s.logger.ErrorContext(ctx, "logo object could not be deleted", slog.String("object_key", key), slog.String("error", err.Error()))
	}
}

// SetLogo replaces the logo of a profile the caller may manage.
func (s *Service) SetLogo(ctx context.Context, p *auth.Principal, kind, slug string, content []byte) (ManagedProfile, error) {
	r, err := s.manageable(ctx, p, kind, slug)
	if err != nil {
		return ManagedProfile{}, err
	}
	if len(content) == 0 {
		return ManagedProfile{}, &ValidationError{Fields: map[string]string{"file": "is required and must not be empty"}}
	}
	if s.files == nil {
		return ManagedProfile{}, errors.New("directory: no file storage configured")
	}
	logo, err := StoreLogo(ctx, s.files, content, s.clock())
	if errors.Is(err, ErrLogoDimensions) {
		return ManagedProfile{}, &ValidationError{Fields: map[string]string{"file": dimensionsMessage}}
	}
	if err != nil {
		return ManagedProfile{}, err
	}
	previous, err := s.store.SetLogo(ctx, r.ID, &logo, logo.UpdatedAt)
	if err != nil {
		s.removeObject(ctx, logo.ObjectKey)
		return ManagedProfile{}, err
	}
	s.removeObject(ctx, previous)
	s.audit(ctx, "profile.logo_changed", p, r, slog.String("content_type", logo.ContentType), slog.Int64("size_bytes", logo.Size))
	r.Logo, r.UpdatedAt = &logo, logo.UpdatedAt
	return managed(r), nil
}

// dimensionsMessage is the per-field message of a logo of the wrong size.
var dimensionsMessage = fmt.Sprintf("must be between %[1]dx%[1]d and %[2]dx%[2]d pixels", MinLogoPixels, MaxLogoPixels)

// RemoveLogo removes the logo of a profile the caller may manage. Removing a
// logo that is not there is not an error.
func (s *Service) RemoveLogo(ctx context.Context, p *auth.Principal, kind, slug string) error {
	r, err := s.manageable(ctx, p, kind, slug)
	if err != nil {
		return err
	}
	previous, err := s.store.SetLogo(ctx, r.ID, nil, s.clock())
	if err != nil {
		return err
	}
	s.removeObject(ctx, previous)
	if previous != "" {
		s.audit(ctx, "profile.logo_removed", p, r)
	}
	return nil
}

// VerifiedOrganisations returns the number of organisations that have a
// public (published and verified) profile of their own.
func (s *Service) VerifiedOrganisations(ctx context.Context) (int, error) {
	return s.store.VerifiedOrganisations(ctx)
}
