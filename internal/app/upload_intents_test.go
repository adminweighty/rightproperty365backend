package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/uploads"
)

type intentBody struct {
	Intent struct {
		ID     string  `json:"id"`
		Status string  `json:"status"`
		Reason *string `json:"reason"`
	} `json:"intent"`
	Upload struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Direct  bool              `json:"direct"`
	} `json:"upload"`
}

// putRaw PUTs raw bytes (the through-API upload of an intent).
func (e *env) putRaw(path string, content []byte, contentType string, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(content))
	req.Header.Set("Content-Type", contentType)
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// API 0.60.0, FR-010: a listing photo through an upload intent: authorised,
// uploaded, checked, used once; its EXIF/GPS is gone like a multipart upload's.
func TestListingPhotoThroughUploadIntent(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	l := e.completeListing(tendai, "House for the upload intent")
	photo := withOrientation(testPhoto(t, 1600, 900), 6)
	sum := sha256.Sum256(photo)
	in := obj{"purpose": uploads.PurposeListingPhoto, "content_type": "image/jpeg", "size_bytes": len(photo), "sha256": hex.EncodeToString(sum[:])}

	wantStatus(t, e.post("/v1/media/upload-intents", in), http.StatusUnauthorized)
	rec := e.post("/v1/media/upload-intents", in, tendai)
	wantStatus(t, rec, http.StatusCreated)
	ib := decode[intentBody](t, rec)
	if ib.Intent.Status != uploads.StatusPendingUpload || ib.Upload.Direct || ib.Upload.Method != http.MethodPut || ib.Upload.Headers["Content-Type"] != "image/jpeg" {
		t.Fatalf("intent %+v", ib)
	}
	// Not uploaded yet: the photo cannot use it.
	wantError(t, e.post("/v1/me/listings/"+l.ID+"/photos", obj{"upload_intent_id": ib.Intent.ID}, tendai), http.StatusConflict, uploads.CodeState)
	wantStatus(t, e.putRaw(ib.Upload.URL, photo, "image/jpeg", tendai), http.StatusNoContent)
	rec = e.post("/v1/media/upload-intents/"+ib.Intent.ID+"/complete", obj{}, tendai)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[intentBody](t, rec); got.Intent.Status != uploads.StatusReady {
		t.Fatalf("complete %+v", got)
	}

	got := managedOf(t, e.post("/v1/me/listings/"+l.ID+"/photos", obj{"upload_intent_id": ib.Intent.ID}, tendai), http.StatusCreated)
	if len(got.Photos) != 1 || got.Photos[0].Width != 900 || got.Photos[0].Height != 1600 {
		t.Fatalf("photos %+v", got.Photos)
	}
	stored := e.get(got.Photos[0].URL, tendai)
	wantStatus(t, stored, http.StatusOK)
	if bytes.Contains(stored.Body.Bytes(), []byte("Exif")) || bytes.Contains(stored.Body.Bytes(), []byte("SecretCamera")) {
		t.Fatal("the stored photo kept the upload's metadata")
	}
	// Used once.
	wantError(t, e.post("/v1/me/listings/"+l.ID+"/photos", obj{"upload_intent_id": ib.Intent.ID}, tendai), http.StatusConflict, uploads.CodeState)

	// A file that is not what it claims is rejected on completion.
	notJPEG := []byte("%PDF-1.4 not a photo at all, padded so the size is right")
	s2 := sha256.Sum256(notJPEG)
	rec = e.post("/v1/media/upload-intents", obj{"purpose": uploads.PurposeListingPhoto, "content_type": "image/jpeg", "size_bytes": len(notJPEG), "sha256": hex.EncodeToString(s2[:])}, tendai)
	wantStatus(t, rec, http.StatusCreated)
	bad := decode[intentBody](t, rec)
	wantStatus(t, e.putRaw(bad.Upload.URL, notJPEG, "image/jpeg", tendai), http.StatusNoContent)
	env := wantError(t, e.post("/v1/media/upload-intents/"+bad.Intent.ID+"/complete", obj{}, tendai), http.StatusUnprocessableEntity, uploads.CodeRejected)
	if env.Error.Fields["reason"] != uploads.ReasonType {
		t.Fatalf("reason %v", env.Error.Fields)
	}
	// Someone else's intent is not found; the multipart upload still works.
	other := newCast(e).as(identity.SeedAgentBEmail)
	wantStatus(t, e.get("/v1/media/upload-intents/"+ib.Intent.ID, other), http.StatusNotFound)
	wantStatus(t, e.uploadPhoto(l.ID, testPhoto(t, 1000, 800), tendai), http.StatusCreated)
}
