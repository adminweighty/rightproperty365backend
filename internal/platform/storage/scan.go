package storage

// Malware scanning (API 0.41.0, BR-014, FR-009/010).
//
// A Storage may scan what users upload: malware.Guard wraps the real
// storage and implements Scanning. The modules never import the scanner;
// they call the helpers below, which fall back to the plain behaviour when
// the storage does not scan (tests, and MALWARE_SCANNER=none behaves exactly
// like the storage alone).
//
//   - Screen scans bytes synchronously before anything is stored (images:
//     the ORIGINAL upload, not the re-encoded copy).
//   - PutUpload stores a document and quarantines it: its scan status is
//     PENDING until the worker has scanned it, and Open refuses it
//     (ErrScanPending) until it is CLEAN.
//   - ScanStatuses reports the status of stored files for the API views.

import (
	"bytes"
	"context"

	"github.com/google/uuid"
)

// Scan statuses of a stored file. A file without a scan record (stored
// while scanning was off, or a file the platform generated itself) has no
// status and is served as before.
const (
	// ScanPending: quarantined, waiting for (or retrying) its scan. Never served.
	ScanPending = "PENDING"
	// ScanClean: scanned, nothing found.
	ScanClean = "CLEAN"
	// ScanInfected: the scanner found something. The object was deleted;
	// the record keeps the signature name. Never served.
	ScanInfected = "INFECTED"
	// ScanLegacyUnscanned: stored before API 0.41.0 and never scanned.
	// Served as before (treated as clean); not scanned on read.
	ScanLegacyUnscanned = "LEGACY_UNSCANNED"
)

// ScanStatusList lists every status, in display order.
func ScanStatusList() []string {
	return []string{ScanPending, ScanClean, ScanInfected, ScanLegacyUnscanned}
}

// Upload kinds: what a quarantined document is, for the logs, the
// administrators' list and the uploader's notice.
const (
	UploadOnboardingDocument = "ONBOARDING_DOCUMENT"
	UploadApplicationFile    = "APPLICATION_FILE"
	UploadLedgerAttachment   = "LEDGER_ATTACHMENT"
	UploadAgreementDocument  = "AGREEMENT_DOCUMENT"
)

// UploadKinds lists the kinds of quarantined documents.
func UploadKinds() []string {
	return []string{UploadOnboardingDocument, UploadApplicationFile, UploadLedgerAttachment, UploadAgreementDocument}
}

// Upload describes a document a user uploaded. It carries no file name and
// no content: only what the scan job needs to tell the uploader.
type Upload struct {
	Kind       string
	UploadedBy uuid.UUID
}

// Scanning is a Storage that scans uploads (malware.Guard).
type Scanning interface {
	Storage
	// Screen scans content synchronously. nil means clean (or scanning is
	// off); otherwise ErrUploadInfected, ErrScannerUnavailable or
	// ErrTooLargeToScan.
	Screen(ctx context.Context, name string, content []byte) error
	// PutUpload stores a document and, when scanning is on, quarantines it
	// (status PENDING). It returns the status ("" when scanning is off).
	PutUpload(ctx context.Context, key string, content []byte, contentType string, u Upload) (string, error)
	// ScanStatuses returns the status of each key that has a scan record.
	ScanStatuses(ctx context.Context, keys []string) (map[string]string, error)
}

// Screen scans content when s scans uploads; otherwise it does nothing.
func Screen(ctx context.Context, s Storage, name string, content []byte) error {
	if sc, ok := s.(Scanning); ok {
		return sc.Screen(ctx, name, content)
	}
	return nil
}

// PutUpload stores a user's document: quarantined until scanned when s
// scans uploads, a plain Put otherwise. It returns the scan status ("" when
// nothing is scanned).
func PutUpload(ctx context.Context, s Storage, key string, content []byte, contentType string, u Upload) (string, error) {
	if sc, ok := s.(Scanning); ok {
		return sc.PutUpload(ctx, key, content, contentType, u)
	}
	return "", s.Put(ctx, key, bytes.NewReader(content), int64(len(content)), contentType)
}

// ScanStatuses returns the scan status of the keys that have one (nil when
// s does not scan). A failure to read them is not fatal to a view: the
// statuses are left out and Open still refuses a file that is not clean.
func ScanStatuses(ctx context.Context, s Storage, keys ...string) map[string]string {
	sc, ok := s.(Scanning)
	if !ok || len(keys) == 0 {
		return nil
	}
	out, err := sc.ScanStatuses(ctx, keys)
	if err != nil {
		return nil
	}
	return out
}

// Servable returns nil when the file may be served or used, and the error
// Open would return otherwise (ErrScanPending, ErrFileInfected). It lets a
// caller refuse an action (sending an agreement for signature) before it
// starts.
func Servable(ctx context.Context, s Storage, key string) error {
	sc, ok := s.(Scanning)
	if !ok {
		return nil
	}
	st, err := sc.ScanStatuses(ctx, []string{key})
	if err != nil {
		return err
	}
	switch st[key] {
	case ScanPending:
		return ErrScanPending
	case ScanInfected:
		return ErrFileInfected
	}
	return nil
}

// PublicError is an error the API answers with its own status and code
// instead of a 500 (httpx.WriteInternalError recognises it). Its message is
// safe to show to the user.
type PublicError struct {
	Status  int
	Code    string
	Message string
}

func (e *PublicError) Error() string { return "storage: " + e.Code }

// HTTPStatus is the response status.
func (e *PublicError) HTTPStatus() int { return e.Status }

// ErrorCode is the error envelope's code.
func (e *PublicError) ErrorCode() string { return e.Code }

// PublicMessage is the error envelope's message.
func (e *PublicError) PublicMessage() string { return e.Message }

// Error codes of the scanning errors.
const (
	CodeFileScanPending    = "FILE_SCAN_PENDING"
	CodeFileInfected       = "FILE_INFECTED"
	CodeScannerUnavailable = "SCANNER_UNAVAILABLE"
	CodeFileTooLargeToScan = "FILE_TOO_LARGE_TO_SCAN"
)

// The scanning errors. Each is answered with its own status (see
// PublicError); wrap them with %w to keep that.
var (
	// ErrScanPending: the file is quarantined until its scan finishes (409).
	ErrScanPending = &PublicError{Status: 409, Code: CodeFileScanPending,
		Message: "This file is still being checked for viruses. Please try again in a minute."}
	// ErrFileInfected: the file failed its scan and was removed (409 on a read).
	ErrFileInfected = &PublicError{Status: 409, Code: CodeFileInfected,
		Message: "This file failed our virus check and was removed. Please upload a clean copy."}
	// ErrUploadInfected: an upload failed its synchronous scan and was not stored (422).
	ErrUploadInfected = &PublicError{Status: 422, Code: CodeFileInfected,
		Message: "This file failed our virus check and was not saved."}
	// ErrScannerUnavailable: the virus scanner could not be reached, so the
	// upload was refused (fail closed, 503). Nothing was stored.
	ErrScannerUnavailable = &PublicError{Status: 503, Code: CodeScannerUnavailable,
		Message: "We cannot check files for viruses right now, so the upload was not saved. Please try again in a few minutes."}
	// ErrTooLargeToScan: larger than the scanner accepts (413). Nothing was stored.
	ErrTooLargeToScan = &PublicError{Status: 413, Code: CodeFileTooLargeToScan,
		Message: "This file is too large to be checked for viruses."}
)
