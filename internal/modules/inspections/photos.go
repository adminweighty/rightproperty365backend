package inspections

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/storage"
)

// ProcessPhoto validates an upload and produces the stored JPEG with the
// maintenance module's rules (type decided by the bytes: JPEG, PNG or
// WebP; 10 MB; 8000 px a side; 40 megapixels; re-encoded at most 2048 px,
// upright, without metadata).
func ProcessPhoto(content []byte) (maintenance.ProcessedPhoto, error) {
	pp, err := maintenance.ProcessPhoto(content)
	switch {
	case errors.Is(err, maintenance.ErrPhotoTooLarge):
		return pp, ErrPhotoTooLarge
	case errors.Is(err, maintenance.ErrPhotoUnsupported):
		return pp, ErrPhotoUnsupported
	case errors.Is(err, maintenance.ErrPhotoDimensions):
		return pp, ErrPhotoDimensions
	}
	return pp, err
}

func (s *Service) removeFiles(ctx context.Context, photos []Photo) {
	if s.files == nil {
		return
	}
	for _, p := range photos {
		if err := s.files.Delete(context.WithoutCancel(ctx), p.ObjectKey); err != nil {
			s.logger.WarnContext(ctx, "could not remove an inspection photo's file", "error", err.Error())
		}
	}
}

func (s *Service) photoOf(ctx context.Context, inspectionID, photoID uuid.UUID) (Photo, error) {
	list, err := s.store.Photos(ctx, inspectionID)
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

func (s *Service) openFile(ctx context.Context, p Photo) (io.ReadCloser, error) {
	if s.files == nil {
		return nil, ErrNoFiles
	}
	rc, err := s.files.Open(ctx, p.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrPhotoNotFound
	}
	return rc, err
}

// AddPhoto serves POST .../inspections/{inspectionId}/items/{itemId}/photos.
// The file is stored first; its row is written after (and the file removed
// again when that fails). A SCHEDULED inspection becomes IN_PROGRESS.
func (s *Service) AddPhoto(ctx context.Context, p *auth.Principal, bookID, id, itemID uuid.UUID, content []byte) (PhotoView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return PhotoView{}, err
	}
	cur, err := s.open(ctx, a, id)
	if err != nil {
		return PhotoView{}, err
	}
	if _, _, ok := cur.Checklist.Item(itemID); !ok {
		return PhotoView{}, ErrItemNotFound
	}
	existing, err := s.store.Photos(ctx, cur.ID)
	if err != nil {
		return PhotoView{}, err
	}
	onItem := 0
	for _, ph := range existing {
		if ph.ItemID == itemID {
			onItem++
		}
	}
	if onItem >= MaxPhotosPerItem || len(existing) >= MaxPhotos {
		return PhotoView{}, ErrPhotoLimit
	}
	if s.files == nil {
		return PhotoView{}, ErrNoFiles
	}
	pp, err := ProcessPhoto(content)
	if err != nil {
		return PhotoView{}, err
	}
	// The ORIGINAL upload is scanned for malware before the re-encoded
	// photo is stored (API 0.41.0).
	if err := storage.Screen(ctx, s.files, "inspection_photo", content); err != nil {
		return PhotoView{}, err
	}
	key, err := storage.NewKey(PhotoKeyPrefix)
	if err != nil {
		return PhotoView{}, err
	}
	if err := s.files.Put(ctx, key, bytes.NewReader(pp.Data), int64(len(pp.Data)), "image/jpeg"); err != nil {
		return PhotoView{}, err
	}
	now := s.clock()
	by := p.UserID
	ph := Photo{ID: newID(), InspectionID: cur.ID, ItemID: itemID, ObjectKey: key, ContentType: "image/jpeg", Width: pp.Width,
		Height: pp.Height, Size: int64(len(pp.Data)), SHA256: pp.SHA256, UploadedBy: &by, UploadedByName: p.Name, UploadedAt: now}
	ch := Change{Photos: []Photo{ph}}
	if cur.Status == StatusScheduled {
		next := cur
		next.UpdatedAt = now
		start(&next, now)
		err = s.save(ctx, cur, next, ch)
	} else {
		ch.Expect = map[uuid.UUID]Expect{cur.ID: {Status: cur.Status, AnyVersion: true}}
		ch.Touch = map[uuid.UUID]time.Time{cur.ID: now}
		err = s.store.Apply(ctx, ch)
	}
	if err != nil {
		s.removeFiles(ctx, []Photo{ph})
		return PhotoView{}, err
	}
	cur.Status = StatusInProgress
	v := &viewer{s: s, a: a}
	return v.photoView(cur, ph), nil
}

// OpenPhoto serves GET .../inspections/{inspectionId}/photos/{photoId}.
func (s *Service) OpenPhoto(ctx context.Context, p *auth.Principal, bookID, id, photoID uuid.UUID) (io.ReadCloser, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return nil, err
	}
	if _, err := s.inspection(ctx, a, id); err != nil {
		return nil, err
	}
	ph, err := s.photoOf(ctx, id, photoID)
	if err != nil {
		return nil, err
	}
	return s.openFile(ctx, ph)
}

// DeletePhoto serves DELETE .../inspections/{inspectionId}/photos/{photoId}.
func (s *Service) DeletePhoto(ctx context.Context, p *auth.Principal, bookID, id, photoID uuid.UUID) error {
	a, err := s.writer(p, bookID)
	if err != nil {
		return err
	}
	cur, err := s.open(ctx, a, id)
	if err != nil {
		return err
	}
	ph, err := s.photoOf(ctx, id, photoID)
	if err != nil {
		return err
	}
	if err := s.store.Apply(ctx, Change{Expect: map[uuid.UUID]Expect{cur.ID: {Status: cur.Status, AnyVersion: true}},
		Touch: map[uuid.UUID]time.Time{cur.ID: s.clock()}, DeletePhotos: []uuid.UUID{ph.ID}}); err != nil {
		return err
	}
	s.removeFiles(ctx, []Photo{ph})
	return nil
}
