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
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"time"

	"golang.org/x/image/draw"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/imaging"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/storage"
)

// A logo is the ONE file of a profile that is public. It is still kept in
// the private object storage and only ever leaves through
// GET /v1/directory/{kind}/{slug}/logo, which serves it for a profile that is
// public right now. Nothing here produces a storage URL.

// Logo limits. MaxLogoBytes, MinLogoPixels and MaxLogoPixels are the limits
// of what may be UPLOADED. What is STORED and served is at most
// LogoStoredPixels on its longer side: a larger upload is scaled down and
// re-encoded once, when it is stored (see PrepareLogo); a smaller one is
// kept exactly as it was uploaded. A logo is never cropped or upscaled, and
// a stored logo is itself within the limits of an upload: no side goes under
// MinLogoPixels (the database refuses a smaller one), so a logo more than
// four times as wide as it is high keeps a longer side above LogoStoredPixels.
const (
	MaxLogoBytes     = 2 << 20 // 2 MB
	MinLogoPixels    = 128
	MaxLogoPixels    = 4096
	LogoStoredPixels = 512
	LogoJPEGQuality  = 85
	LogoKeyPrefix    = "logos"
	LogoCacheMaxAge  = 5 * time.Minute
	LogoCacheStale   = 24 * time.Hour
	// LogoCacheVersioned is how long a logo requested by the URL of its
	// current version (?v=) may be cached: that URL never serves another image.
	LogoCacheVersioned = 365 * 24 * time.Hour
	logoContentPNG     = "image/png"
	logoContentJPEG    = "image/jpeg"
	maxLogoBodyExtra   = 64 << 10
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

// LogoImage describes an image: the uploaded one (CheckLogo) or the one to
// store (PrepareLogo).
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
// decoded, to learn the size in pixels. The result describes the content as
// uploaded; what is stored may be smaller (PrepareLogo).
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

// PrepareLogo validates an uploaded image (CheckLogo) and returns the bytes
// to store with their description. An image whose longer side is more than
// LogoStoredPixels is scaled down to fit a square of that size (same
// proportions, Catmull-Rom) - but never so far that its shorter side goes
// under MinLogoPixels: a 1200x200 image becomes 768x128, not 512x85. It is
// encoded again in its own format: PNG stays PNG, so transparency survives,
// and JPEG stays JPEG at LogoJPEGQuality, turned upright first when its EXIF
// data says the camera was held sideways (encoding again drops that data).
// Any other image is returned as it is, byte for byte.
//
// The image is fully decoded only after CheckLogo accepted it, so at most
// MaxLogoBytes of input and MaxLogoPixels each way are ever decoded. An image
// whose header is valid but whose data cannot be decoded is ErrLogoUnsupported.
func PrepareLogo(content []byte) (LogoImage, []byte, error) {
	img, err := CheckLogo(content)
	if err != nil {
		return LogoImage{}, nil, err
	}
	// The scale is num/den: the longer side to LogoStoredPixels, or, when that
	// would make the shorter side too small, the shorter side to MinLogoPixels.
	longer, shorter := max(img.Width, img.Height), min(img.Width, img.Height)
	num, den := LogoStoredPixels, longer
	if shorter*num < MinLogoPixels*den {
		num, den = MinLogoPixels, shorter
	}
	if num >= den {
		return img, content, nil // small enough already: never upscaled
	}
	src, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return LogoImage{}, nil, ErrLogoUnsupported
	}
	scaled := func(side int) int { return (side*num + den/2) / den } // to the nearest pixel
	dst := image.NewRGBA(image.Rect(0, 0, scaled(img.Width), scaled(img.Height)))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)

	var out bytes.Buffer
	if img.ContentType == logoContentJPEG {
		dst = upright(dst, jpegOrientation(content))
		err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: LogoJPEGQuality})
	} else {
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, dst)
	}
	if err != nil {
		return LogoImage{}, nil, fmt.Errorf("encode logo: %w", err)
	}
	if out.Len() > MaxLogoBytes {
		// Cannot happen with real images (fewer pixels, same format). The
		// upload is within every limit, so it is kept rather than refused.
		return img, content, nil
	}
	sum := sha256.Sum256(out.Bytes())
	return LogoImage{ContentType: img.ContentType, Width: dst.Bounds().Dx(), Height: dst.Bounds().Dy(),
		Checksum: hex.EncodeToString(sum[:])}, out.Bytes(), nil
}

// jpegOrientation returns the EXIF orientation of a JPEG (1 to 8), or 1 when
// it has none or it cannot be read (imaging.JPEGOrientation).
func jpegOrientation(content []byte) int { return imaging.JPEGOrientation(content) }

// upright returns img turned the way its EXIF orientation says it is to be
// shown (imaging.Upright).
func upright(img *image.RGBA, orientation int) *image.RGBA { return imaging.Upright(img, orientation) }

// StoreLogo validates an image, scales it down when it is larger than a logo
// needs to be (PrepareLogo) and stores it under a new random key. The Logo
// returned describes the STORED image. The caller attaches it to a profile,
// and removes the object again if that fails.
//
// The ORIGINAL image is scanned for malware before anything is stored (API
// 0.41.0): a small logo is stored byte for byte, so this matters here.
func StoreLogo(ctx context.Context, files storage.Storage, content []byte, now time.Time) (Logo, error) {
	original := content
	img, content, err := PrepareLogo(content)
	if err != nil {
		return Logo{}, err
	}
	if err := storage.Screen(ctx, files, "logo", original); err != nil {
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
