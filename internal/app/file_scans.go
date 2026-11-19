package app

import (
	"context"
	"errors"

	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/developments"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/inspections"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/malware"
)

// FileScanAdminRoles may read GET /v1/admin/file-scans.
func FileScanAdminRoles() []string { return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin} }

// FileScanNotifier tells the uploader of an infected file, in-app: a
// SECURITY notice (essential, so it cannot be switched off) with safe text
// only (no file name, no signature name), once per scan record.
func FileScanNotifier(inbox *inapp.Service) malware.Notifier {
	return func(ctx context.Context, r malware.Record) error {
		if inbox == nil {
			return errors.New("no notification centre")
		}
		if r.UploadedBy == nil {
			return nil
		}
		_, _, err := inbox.Notify(ctx, inapp.Draft{SourceEventID: malware.NoticeSourceID(r), UserID: *r.UploadedBy, Type: inapp.TypeSecurity,
			Title: malware.NoticeTitle, Body: malware.NoticeBody, Link: "/account"})
		return err
	}
}

// UnscannedKeyPrefixes are the object key prefixes that never have a scan
// record: images (screened before they are stored) and the PDFs the
// platform renders. The guard skips the scan-record lookup for them.
func UnscannedKeyPrefixes() []string {
	return []string{listings.PhotoKeyPrefix, developments.PhotoKeyPrefix, directory.LogoKeyPrefix, maintenance.PhotoKeyPrefix, inspections.PhotoKeyPrefix,
		agreements.SignatureKeyPrefix, agreements.SavedSignatureKeyPrefix, leases.ObjectKeyPrefix}
}
