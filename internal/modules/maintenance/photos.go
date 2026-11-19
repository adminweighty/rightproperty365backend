package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // PNG uploads
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // WebP uploads (decoder only: everything is stored as JPEG)

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/imaging"
	"rightproperty365.com/backend/internal/platform/storage"
)

// ProcessedPhoto is an upload turned into what is stored.
type ProcessedPhoto struct {
	Data          []byte
	Width, Height int
	SHA256        string
}

// ProcessPhoto validates an upload and produces the stored JPEG. The type is
// decided by the bytes alone (sniffed, then the header decoded: never a file
// name or a Content-Type header) and must be JPEG, PNG (not animated) or
// WebP; the limits are checked on the header before anything is decoded in
// full. A JPEG is turned upright by its EXIF orientation, transparency is
// flattened onto white, the longer side is brought down to StoredPhotoSide,
// and the result is encoded as a new JPEG, which drops all metadata (EXIF,
// GPS, comments).
func ProcessPhoto(content []byte) (ProcessedPhoto, error) {
	if len(content) > MaxPhotoBytes {
		return ProcessedPhoto{}, ErrPhotoTooLarge
	}
	want := map[string]string{"image/jpeg": "jpeg", "image/png": "png", "image/webp": "webp"}[http.DetectContentType(content)]
	if want == "" {
		return ProcessedPhoto{}, ErrPhotoUnsupported
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || format != want {
		return ProcessedPhoto{}, ErrPhotoUnsupported
	}
	if format == "png" && imaging.AnimatedPNG(content) {
		return ProcessedPhoto{}, ErrPhotoUnsupported
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxPhotoSide || cfg.Height > MaxPhotoSide || cfg.Width*cfg.Height > MaxPhotoPixels {
		return ProcessedPhoto{}, ErrPhotoDimensions
	}
	src, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return ProcessedPhoto{}, ErrPhotoUnsupported
	}
	orientation := 1
	if format == "jpeg" {
		orientation = imaging.JPEGOrientation(content)
	}
	w, h := cfg.Width, cfg.Height
	if longer := max(w, h); longer > StoredPhotoSide {
		w, h = max(1, (w*StoredPhotoSide+longer/2)/longer), max(1, (h*StoredPhotoSide+longer/2)/longer)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	dst = imaging.Upright(dst, orientation)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: PhotoJPEGQuality}); err != nil {
		return ProcessedPhoto{}, fmt.Errorf("encode photo: %w", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return ProcessedPhoto{Data: buf.Bytes(), Width: dst.Bounds().Dx(), Height: dst.Bounds().Dy(), SHA256: hex.EncodeToString(sum[:])}, nil
}

// addPhoto stores the file, then writes its row and PHOTO_ADDED item (the
// file is removed again when the write fails).
func (s *Service) addPhoto(ctx context.Context, r Request, x actor, content []byte) (Photo, error) {
	if !Open(r.Status) {
		return Photo{}, ErrClosed
	}
	if r.PhotoCount >= MaxPhotos {
		return Photo{}, ErrPhotoLimit
	}
	if s.files == nil {
		return Photo{}, ErrNoFiles
	}
	pp, err := ProcessPhoto(content)
	if err != nil {
		return Photo{}, err
	}
	// The ORIGINAL upload is scanned for malware before the re-encoded
	// photo is stored (API 0.41.0).
	if err := storage.Screen(ctx, s.files, "maintenance_photo", content); err != nil {
		return Photo{}, err
	}
	key, err := storage.NewKey(PhotoKeyPrefix)
	if err != nil {
		return Photo{}, err
	}
	if err := s.files.Put(ctx, key, bytes.NewReader(pp.Data), int64(len(pp.Data)), "image/jpeg"); err != nil {
		return Photo{}, err
	}
	now := s.clock()
	by := x.userID
	ph := Photo{ID: newID(), RequestID: r.ID, ObjectKey: key, ContentType: "image/jpeg", Width: pp.Width, Height: pp.Height,
		Size: int64(len(pp.Data)), SHA256: pp.SHA256, UploaderKind: x.kind, UploadedBy: &by, UploadedByName: x.name, UploadedAt: now}
	ev := x.event(r.ID, EventPhotoAdded, now)
	ev.Details = map[string]any{"photo_id": ph.ID.String()}
	if err := s.store.Apply(ctx, Change{Touch: map[uuid.UUID]time.Time{r.ID: now}, Photos: []Photo{ph}, Events: []Event{ev}}); err != nil {
		if dErr := s.files.Delete(context.WithoutCancel(ctx), key); dErr != nil {
			s.logger.WarnContext(ctx, "could not remove an unused maintenance photo", "error", dErr.Error())
		}
		return Photo{}, err
	}
	return ph, nil
}

func (s *Service) photoOf(ctx context.Context, requestID, photoID uuid.UUID) (Photo, error) {
	list, err := s.store.Photos(ctx, requestID)
	if err != nil {
		return Photo{}, err
	}
	for _, p := range list {
		if p.ID == photoID {
			return p, nil
		}
	}
	return Photo{}, ErrPhotoNotFound
}

func (s *Service) open(ctx context.Context, p Photo) (io.ReadCloser, error) {
	if s.files == nil {
		return nil, ErrNoFiles
	}
	rc, err := s.files.Open(ctx, p.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrPhotoNotFound
	}
	return rc, err
}

// AddPhoto serves POST .../maintenance/{requestId}/photos.
func (s *Service) AddPhoto(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, content []byte) (PhotoView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return PhotoView{}, err
	}
	r, err := s.request(ctx, a, id)
	if err != nil {
		return PhotoView{}, err
	}
	ph, err := s.addPhoto(ctx, r, managerActor(p), content)
	if err != nil {
		return PhotoView{}, err
	}
	v := &viewer{s: s, a: a}
	return v.photoView(r, ph), nil
}

// OpenPhoto serves GET .../maintenance/{requestId}/photos/{photoId}.
func (s *Service) OpenPhoto(ctx context.Context, p *auth.Principal, bookID, id, photoID uuid.UUID) (io.ReadCloser, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return nil, err
	}
	if _, err := s.request(ctx, a, id); err != nil {
		return nil, err
	}
	ph, err := s.photoOf(ctx, id, photoID)
	if err != nil {
		return nil, err
	}
	return s.open(ctx, ph)
}

// DeletePhoto serves DELETE .../maintenance/{requestId}/photos/{photoId}.
func (s *Service) DeletePhoto(ctx context.Context, p *auth.Principal, bookID, id, photoID uuid.UUID) error {
	a, err := s.writer(p, bookID)
	if err != nil {
		return err
	}
	r, err := s.request(ctx, a, id)
	if err != nil {
		return err
	}
	ph, err := s.photoOf(ctx, id, photoID)
	if err != nil {
		return err
	}
	if !Open(r.Status) {
		return ErrClosed
	}
	if err := s.store.Apply(ctx, Change{Touch: map[uuid.UUID]time.Time{r.ID: s.clock()}, DeletePhotos: []uuid.UUID{ph.ID}}); err != nil {
		return err
	}
	if s.files != nil {
		if err := s.files.Delete(context.WithoutCancel(ctx), ph.ObjectKey); err != nil {
			s.logger.WarnContext(ctx, "could not remove a deleted maintenance photo's file", "error", err.Error())
		}
	}
	return nil
}
