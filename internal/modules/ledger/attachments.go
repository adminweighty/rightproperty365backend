package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Attachments are evidence (receipts, statements): pdf, jpg, png and webp,
// identified by their content (the same sniffing as onboarding documents
// and application files), at most 10 MB, kept in the private object
// storage and only ever streamed back through the API (audited).
var attachmentTypes = map[string]string{
	applications.FilePDF:  "application/pdf",
	applications.FileJPG:  "image/jpeg",
	applications.FilePNG:  "image/png",
	applications.FileWEBP: "image/webp",
}

var attachmentExt = map[string]string{
	applications.FilePDF: ".pdf", applications.FileJPG: ".jpg", applications.FilePNG: ".png", applications.FileWEBP: ".webp",
}

// AcceptedFileKinds are the attachment types, for messages.
func AcceptedFileKinds() []string {
	return []string{applications.FilePDF, applications.FileJPG, applications.FilePNG, applications.FileWEBP}
}

// fileName keeps the client's base name (printable, at most 200 runes) with
// the extension of the sniffed type.
func fileName(name, kind string) string {
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, base)
	base = strings.TrimSpace(base)
	if base == "" || base == "." {
		base = "attachment"
	}
	if utf8.RuneCountInString(base) > 190 {
		base = string([]rune(base)[:190])
	}
	return base + attachmentExt[kind]
}

// AddAttachment serves POST .../entries/{id}/attachments.
func (s *Service) AddAttachment(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, name string, content []byte) (AttachmentView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return AttachmentView{}, err
	}
	e, err := s.entry(ctx, a, id)
	if err != nil {
		return AttachmentView{}, err
	}
	if !a.mayAnnotate(e) {
		return AttachmentView{}, ErrForbidden
	}
	switch {
	case len(content) == 0:
		return AttachmentView{}, &ValidationError{Fields: map[string]string{"file": "is required and must not be empty"}}
	case len(content) > MaxFileBytes:
		return AttachmentView{}, ErrFileTooLarge
	}
	kind := applications.Sniff(content)
	contentType, ok := attachmentTypes[kind]
	if !ok {
		return AttachmentView{}, ErrUnsupportedFile
	}
	existing, err := s.store.Attachments(ctx, e.ID)
	if err != nil {
		return AttachmentView{}, err
	}
	if len(existing) >= MaxAttachments {
		return AttachmentView{}, &ValidationError{Fields: map[string]string{"file": fmt.Sprintf("an entry holds at most %d attachments", MaxAttachments)}}
	}
	if s.files == nil {
		return AttachmentView{}, errors.New("ledger: no file storage")
	}
	key, err := storage.NewKey(AttachmentKeyPrefix)
	if err != nil {
		return AttachmentView{}, err
	}
	sum := sha256.Sum256(content)
	by := p.UserID
	att := Attachment{ID: newID(), EntryID: e.ID, FileName: fileName(name, kind), ContentType: contentType, Size: int64(len(content)),
		SHA256: hex.EncodeToString(sum[:]), ObjectKey: key, UploadedBy: &by, UploadedByName: p.Name, UploadedAt: s.clock()}
	// Quarantined until scanned when malware scanning is on (API 0.41.0).
	if att.ScanStatus, err = storage.PutUpload(ctx, s.files, key, content, contentType,
		storage.Upload{Kind: storage.UploadLedgerAttachment, UploadedBy: p.UserID}); err != nil {
		return AttachmentView{}, fmt.Errorf("store ledger attachment: %w", err)
	}
	ch := Change{Attachments: []Attachment{att},
		Audit: s.event(ctx, a, orgaudit.ActionLedgerAttachmentAdded, orgaudit.TargetLedgerEntry, e.ID, Label(e), nil,
			map[string]any{"attachment_id": att.ID.String(), "content_type": contentType, "size": att.Size})}
	if err := s.store.Apply(ctx, ch); err != nil {
		if derr := s.files.Delete(context.WithoutCancel(ctx), key); derr != nil {
			s.logger.WarnContext(ctx, "could not remove a ledger attachment after a failed upload", slog.String("error", derr.Error()))
		}
		return AttachmentView{}, err
	}
	return attachmentView(a.book, att), nil
}

// FileContent is an open attachment.
type FileContent struct {
	Attachment Attachment
	Content    io.ReadCloser
}

// OpenAttachment serves GET .../entries/{id}/attachments/{attachmentId}:
// any reader of the book, and every download is audited.
func (s *Service) OpenAttachment(ctx context.Context, p *auth.Principal, bookID, id, attachmentID uuid.UUID) (FileContent, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return FileContent{}, err
	}
	e, err := s.entry(ctx, a, id)
	if err != nil {
		return FileContent{}, err
	}
	list, err := s.store.Attachments(ctx, e.ID)
	if err != nil {
		return FileContent{}, err
	}
	i := slices.IndexFunc(list, func(x Attachment) bool { return x.ID == attachmentID })
	if i < 0 || s.files == nil {
		return FileContent{}, ErrAttachmentNotFound
	}
	att := list[i]
	rc, err := s.files.Open(ctx, att.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		return FileContent{}, ErrAttachmentNotFound
	}
	if err != nil {
		return FileContent{}, err
	}
	ch := Change{Audit: s.event(ctx, a, orgaudit.ActionLedgerAttachmentViewed, orgaudit.TargetLedgerEntry, e.ID, Label(e), nil,
		map[string]any{"attachment_id": att.ID.String()})}
	if len(ch.Audit) > 0 {
		if err := s.store.Apply(ctx, ch); err != nil {
			_ = rc.Close()
			return FileContent{}, err
		}
	}
	return FileContent{Attachment: att, Content: rc}, nil
}
