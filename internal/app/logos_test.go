package app

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// ---------------------------------------------------------------------------
// Test images
// ---------------------------------------------------------------------------

func flatImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 0x20, G: 0x60, B: 0xc0, A: 0xff})
		}
	}
	return img
}

func logoPNG(w, h int) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, flatImage(w, h))
	return buf.Bytes()
}

func logoJPEG(w, h int) []byte {
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, flatImage(w, h), nil)
	return buf.Bytes()
}

func logoGIF(w, h int) []byte {
	var buf bytes.Buffer
	_ = gif.Encode(&buf, flatImage(w, h), nil)
	return buf.Bytes()
}

// noisyPNG is a valid PNG of random pixels: it does not compress, so a
// 1024x1024 one is larger than the 2 MB limit.
func noisyPNG(side int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	rnd := rand.New(rand.NewSource(1))
	_, _ = rnd.Read(img.Pix)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// animatedPNG is a valid PNG with an animation control chunk (acTL) after
// its header, which is what makes a PNG an animated one.
func animatedPNG() []byte {
	base := logoPNG(200, 200)
	const header = 8 + 25 // signature + IHDR chunk
	data := make([]byte, 8)
	binary.BigEndian.PutUint32(data[0:4], 2) // two frames
	chunk := append([]byte("acTL"), data...)
	var out bytes.Buffer
	out.Write(base[:header])
	_ = binary.Write(&out, binary.BigEndian, uint32(len(data)))
	out.Write(chunk)
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	out.Write(base[header:])
	return out.Bytes()
}

const testSVG = `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="200" height="200"><script>alert(1)</script></svg>`

// A file that says WebP in its first bytes.
var testWebP = append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 64)...)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// putLogo sends the multipart PUT a client's form sends. The part's declared
// content type is whatever the form says: the API ignores it.
func (e *env) putLogo(kind, slug string, content []byte, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if content != nil {
		part, err := w.CreateFormFile("file", "logo.bin")
		if err != nil {
			e.t.Fatal(err)
		}
		_, _ = part.Write(content)
	}
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPut, "/v1/me/profiles/"+kind+"/"+slug+"/logo", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	e.seq++
	req.RemoteAddr = fmt.Sprintf("198.51.100.%d:40000", e.seq%250) // spread over the upload rate limit
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

type logoBody struct {
	URL    string `json:"url"`
	Width  *int   `json:"width"`
	Height *int   `json:"height"`
}

type managedBody struct {
	Kind     string    `json:"kind"`
	Slug     string    `json:"slug"`
	Name     string    `json:"name"`
	Verified bool      `json:"verified"`
	Logo     *logoBody `json:"logo"`
}

func (e *env) myProfiles(opts ...reqOpt) []managedBody {
	e.t.Helper()
	rec := e.get("/v1/me/profiles", opts...)
	wantStatus(e.t, rec, http.StatusOK)
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		e.t.Fatalf("GET /v1/me/profiles Cache-Control = %q", cc)
	}
	return decode[struct {
		Profiles []managedBody `json:"profiles"`
	}](e.t, rec).Profiles
}

func addresses(profiles []managedBody) string {
	out := make([]string, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p.Kind+"/"+p.Slug)
	}
	return strings.Join(out, ",")
}

// profileLogo returns the `logo` of a public profile (nil when it says null).
func (e *env) profileLogo(kind, slug string) *logoBody {
	e.t.Helper()
	rec := e.get("/v1/directory/" + kind + "/" + slug)
	wantStatus(e.t, rec, http.StatusOK)
	return decode[struct {
		Profile struct {
			Logo *logoBody `json:"logo"`
		} `json:"profile"`
	}](e.t, rec).Profile.Logo
}

func (e *env) listing(slug string) listings.Listing {
	e.t.Helper()
	rec := e.get("/v1/listings/" + slug)
	wantStatus(e.t, rec, http.StatusOK)
	return decode[struct {
		Listing listings.Listing `json:"listing"`
	}](e.t, rec).Listing
}

// ---------------------------------------------------------------------------
// The public route
// ---------------------------------------------------------------------------

// The cache policy of a logo: a year for the URL of the current version (the
// one the API hands out), minutes for any other address of the same image.
const (
	logoCacheControl          = "public, max-age=300, stale-while-revalidate=86400"
	logoCacheControlVersioned = "public, max-age=31536000, immutable"
)

func TestPublicLogoRoute(t *testing.T) {
	e := newEnv(t)
	logo := e.profileLogo("agencies", "sample-estates")
	if logo == nil || logo.Width == nil || *logo.Width != devseed.SampleLogoSize || logo.Height == nil || *logo.Height != devseed.SampleLogoSize {
		t.Fatalf("logo = %+v", logo)
	}
	sample := devseed.SampleLogos()["agencies/sample-estates"]
	// A path relative to the API origin, versioned by the image's checksum.
	if want := "/v1/directory/agencies/sample-estates/logo?v=" + sample.Logo.Checksum[:listings.LogoVersionLength]; logo.URL != want {
		t.Fatalf("url = %q, want %q", logo.URL, want)
	}

	rec := e.get(logo.URL)
	wantStatus(t, rec, http.StatusOK)
	head := rec.Header()
	etag := head.Get("ETag")
	if head.Get("Content-Type") != "image/png" || head.Get("Cache-Control") != logoCacheControlVersioned || etag != `"`+sample.Logo.Checksum+`"` ||
		head.Get("X-Content-Type-Options") != "nosniff" || head.Get("Content-Length") != fmt.Sprint(len(sample.Content)) {
		t.Fatalf("headers = %v", head)
	}
	if !bytes.Equal(rec.Body.Bytes(), sample.Content) {
		t.Fatal("the image served is not the image stored")
	}
	if _, err := png.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
		t.Fatalf("the sample logo is not a PNG: %v", err)
	}
	// No session is read and none is set: the same for everyone.
	if head.Get("Set-Cookie") != "" || strings.Contains(strings.ToLower(strings.Join(head.Values("Vary"), ",")), "cookie") {
		t.Fatalf("the logo route touches cookies: %v", head)
	}
	signedIn := e.get(logo.URL, withCookie(e.loginCookie(identity.SeedConsumerEmail, seedPassword)))
	if signedIn.Code != http.StatusOK || signedIn.Header().Get("ETag") != etag || !bytes.Equal(signedIn.Body.Bytes(), sample.Content) {
		t.Fatal("the logo differs for a signed-in caller")
	}
	// The version is only a cache key: the route serves the current logo with
	// or without it, and with the version of another image. Only the URL of
	// the current version may be kept for a year: anything else is kept for
	// minutes, so that an image is never pinned to an address that is not its own.
	const bare = "/v1/directory/agencies/sample-estates/logo"
	version := sample.Logo.Checksum[:listings.LogoVersionLength]
	for _, path := range []string{
		bare,                                // no version
		bare + "?v=",                        // an empty one
		bare + "?v=000000000000",            // the version of another image
		bare + "?v=" + version[:6],          // only the start of the current one
		bare + "?v=" + sample.Logo.Checksum, // more than the version
		bare + "?v=" + strings.ToUpper(version) + "x",
		bare + "?version=" + version,
	} {
		rec := e.get(path)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != logoCacheControl || rec.Header().Get("ETag") != etag ||
			!bytes.Equal(rec.Body.Bytes(), sample.Content) {
			t.Fatalf("GET %s: %d Cache-Control=%q", path, rec.Code, rec.Header().Get("Cache-Control"))
		}
		not := e.get(path, withHeader("If-None-Match", etag))
		if not.Code != http.StatusNotModified || not.Header().Get("Cache-Control") != logoCacheControl {
			t.Fatalf("GET %s If-None-Match: %d Cache-Control=%q", path, not.Code, not.Header().Get("Cache-Control"))
		}
	}

	// 304 when the client already has it, with the same policy as the 200.
	for _, header := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		not := e.get(logo.URL, withHeader("If-None-Match", header))
		if not.Code != http.StatusNotModified || not.Body.Len() != 0 || not.Header().Get("ETag") != etag || not.Header().Get("Cache-Control") != logoCacheControlVersioned {
			t.Fatalf("If-None-Match %s: %d %v", header, not.Code, not.Header())
		}
	}
	wantStatus(t, e.get(logo.URL, withHeader("If-None-Match", `"something-else"`)), http.StatusOK)

	// 404: a profile without a logo, an agent without a photo, an unknown
	// profile, an unknown kind.
	if e.profileLogo("agencies", "sample-realty") != nil || devseed.NoLogoProfile != "agencies/sample-realty" {
		t.Fatal("the sample agency that must have no logo has one")
	}
	wantError(t, e.get("/v1/directory/agencies/sample-realty/logo"), http.StatusNotFound, directory.CodeLogoNotFound)
	wantError(t, e.get("/v1/directory/agents/tendai-ncube/logo"), http.StatusNotFound, directory.CodeLogoNotFound)
	wantError(t, e.get("/v1/directory/agencies/nobody/logo"), http.StatusNotFound, directory.CodeProfileNotFound)
	wantError(t, e.get("/v1/directory/nothing/sample-estates/logo"), http.StatusNotFound, directory.CodeKindNotFound)

	// 404 for a profile that is not public, although its logo is still stored:
	// unverified, then suspended.
	for _, hide := range []func(r *directory.Record){
		func(r *directory.Record) { r.Verified = false },
		func(r *directory.Record) { r.Status = "SUSPENDED" },
	} {
		r, err := e.directory.Find(t.Context(), "agencies", "sample-estates")
		if err != nil {
			t.Fatal(err)
		}
		hidden := r
		hide(&hidden)
		e.directory.Put(hidden)
		wantError(t, e.get(logo.URL), http.StatusNotFound, directory.CodeProfileNotFound)
		wantError(t, e.get(logo.URL, withHeader("If-None-Match", etag)), http.StatusNotFound, directory.CodeProfileNotFound)
		e.directory.Put(r)
		wantStatus(t, e.get(logo.URL), http.StatusOK)
	}
}

// Every sample business has a logo except one; no sample agent has a photo.
func TestSampleLogos(t *testing.T) {
	e := newEnv(t)
	with, without := 0, []string{}
	for _, p := range devseed.Sample().Profiles {
		logo := e.profileLogo(p.Kind, p.Slug)
		switch {
		case logo != nil && p.Kind == "agents":
			t.Fatalf("sample agent %s has a photo", p.Slug)
		case logo != nil:
			with++
			rec := e.get(logo.URL)
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("%s/%s logo: %d %s", p.Kind, p.Slug, rec.Code, rec.Header().Get("Content-Type"))
			}
			if img, err := directory.CheckLogo(rec.Body.Bytes()); err != nil || img.Width != 256 || img.Height != 256 {
				t.Fatalf("%s/%s logo: %+v %v", p.Kind, p.Slug, img, err)
			}
		case p.Kind != "agents":
			without = append(without, p.Kind+"/"+p.Slug)
		}
	}
	if with != len(devseed.SampleLogos()) || with < 20 || len(without) != 1 || without[0] != devseed.NoLogoProfile {
		t.Fatalf("%d sample logos, businesses without one: %v", with, without)
	}
	// Different businesses, different images.
	seen := map[string]string{}
	for key, l := range devseed.SampleLogos() {
		if other, dup := seen[l.Logo.Checksum]; dup {
			t.Fatalf("%s and %s share a logo", key, other)
		}
		seen[l.Logo.Checksum] = key
	}
	if devseed.Initials("Sample Estates") != "SE" || devseed.Initials("Sample Law Chambers") != "SL" || devseed.Initials("3rd Street") != "S" {
		t.Fatal("initials")
	}
}

// ---------------------------------------------------------------------------
// Uploading a logo at onboarding
// ---------------------------------------------------------------------------

func TestLogoAtOnboarding(t *testing.T) {
	e := newEnv(t)
	const role = "AGENCY_OWNER"
	c := e.selfRegister("logo.agency@example.com", "CONSUMER", role)
	wantStatus(t, e.put("/v1/me/onboarding/"+role, detailsFor(role, "Jacaranda Homes"), browser(c)), http.StatusOK)
	for _, doc := range requiredDocuments(role) {
		wantStatus(t, e.upload(c, role, doc, "scan.pdf", testPDF()), http.StatusCreated)
	}

	// The applicant is told that this one document becomes public.
	requirements := e.get("/v1/onboarding/requirements").Body.String()
	if !strings.Contains(requirements, `"code":"LOGO","label":"Logo or profile photo"`) || !strings.Contains(requirements, "shown PUBLICLY") ||
		!strings.Contains(onboarding.LogoDescription, "PNG or JPEG, at most 2 MB") {
		t.Fatalf("the LOGO document is not described: %s", requirements)
	}
	// It is optional: the application could be submitted without it.
	if app := e.myApp(c, role); !app.CanSubmit {
		t.Fatalf("an application without a logo cannot be submitted: %+v", app.Missing)
	}
	// A role without a public profile cannot attach one.
	staff := e.selfRegister("logo.staff@example.com", "CONSUMER", "AGENCY_STAFF")
	wantError(t, e.upload(staff, "AGENCY_STAFF", "LOGO", "me.png", logoPNG(200, 200)), http.StatusBadRequest, httpx.CodeValidationFailed)

	// Refused: everything that is not a still PNG or JPEG of a sensible size.
	files := e.storedFiles()
	for name, tt := range map[string]struct {
		content []byte
		status  int
		code    string
	}{
		"logo.svg":      {[]byte(testSVG), http.StatusUnsupportedMediaType, onboarding.CodeUnsupportedFile},
		"logo.gif":      {logoGIF(200, 200), http.StatusUnsupportedMediaType, onboarding.CodeUnsupportedFile},
		"logo.webp":     {testWebP, http.StatusUnsupportedMediaType, onboarding.CodeUnsupportedFile},
		"logo.pdf":      {testPDF(), http.StatusUnsupportedMediaType, onboarding.CodeUnsupportedFile},
		"text.png":      {[]byte("this is a text file with a .png name\n"), http.StatusUnsupportedMediaType, onboarding.CodeUnsupportedFile},
		"animated.png":  {animatedPNG(), http.StatusUnsupportedMediaType, onboarding.CodeUnsupportedFile},
		"truncated.png": {logoPNG(200, 200)[:20], http.StatusUnsupportedMediaType, onboarding.CodeUnsupportedFile},
		"small.png":     {logoPNG(127, 200), http.StatusBadRequest, httpx.CodeValidationFailed},
		"small.jpg":     {logoJPEG(200, 100), http.StatusBadRequest, httpx.CodeValidationFailed},
		"wide.png":      {logoPNG(4097, 128), http.StatusBadRequest, httpx.CodeValidationFailed},
		"heavy.png":     {noisyPNG(1024), http.StatusRequestEntityTooLarge, onboarding.CodeFileTooLarge},
	} {
		env := wantError(t, e.upload(c, role, "LOGO", name, tt.content), tt.status, tt.code)
		if tt.status == http.StatusBadRequest && !strings.Contains(env.Error.Fields["file"], "128x128 and 4096x4096") {
			t.Fatalf("%s: fields = %v", name, env.Error.Fields)
		}
		if tt.status == http.StatusRequestEntityTooLarge && !strings.Contains(env.Error.Message, "2 MB") {
			t.Fatalf("%s: message = %q", name, env.Error.Message)
		}
	}
	if len(noisyPNG(1024)) <= directory.MaxLogoBytes {
		t.Fatal("test setup: the heavy PNG is not over the limit")
	}
	if e.storedFiles() != files {
		t.Fatal("a refused logo was stored")
	}
	// The other documents keep their own rule: a PDF is still fine there.
	wantStatus(t, e.upload(c, role, "TAX_CLEARANCE", "tax.pdf", testPDF()), http.StatusCreated)

	// Accepted: a PNG, then a JPEG that replaces it. The smallest and the
	// largest sizes are inside the limits.
	for _, ok := range [][]byte{logoPNG(128, 128), logoPNG(4096, 128), logoPNG(400, 300)} {
		wantStatus(t, e.upload(c, role, "LOGO", "logo.png", ok), http.StatusCreated)
	}
	photo := logoJPEG(300, 200)
	rec := e.upload(c, role, "LOGO", "our logo.jpeg", photo)
	wantStatus(t, rec, http.StatusCreated)
	doc := decode[struct {
		Document docBody `json:"document"`
	}](t, rec).Document
	if doc.Type.Code != "LOGO" || doc.ContentType != "image/jpeg" || doc.FileName != "our logo.jpg" {
		t.Fatalf("document = %+v", doc)
	}
	app := e.myApp(c, role)
	logos := 0
	for _, d := range app.Documents {
		if d.Type.Code == "LOGO" {
			logos++
		}
	}
	if logos != 1 {
		t.Fatalf("%d LOGO documents, want the newest only", logos)
	}

	// Nothing is public before approval.
	wantError(t, e.get("/v1/directory/agencies/jacaranda-homes"), http.StatusNotFound, directory.CodeProfileNotFound)
	wantError(t, e.get("/v1/directory/agencies/jacaranda-homes/logo"), http.StatusNotFound, directory.CodeProfileNotFound)

	wantStatus(t, e.post("/v1/me/onboarding/"+role+"/submit", nil, browser(c)), http.StatusOK)
	admin := e.admin()
	// The reviewer sees the logo like every other document, and can open it.
	detail := decode[struct {
		Application adminApplicationBody `json:"application"`
	}](t, e.get("/v1/admin/onboarding/"+app.ID, withCookie(admin))).Application
	var logoDoc *docBody
	for i, d := range detail.Documents {
		if d.Type.Code == "LOGO" {
			logoDoc = &detail.Documents[i]
		}
	}
	if logoDoc == nil {
		t.Fatalf("the review detail does not list the logo: %+v", detail.Documents)
	}
	file := e.get("/v1/admin/onboarding/"+app.ID+"/documents/"+logoDoc.ID+"/file", withCookie(admin))
	if file.Code != http.StatusOK || !bytes.Equal(file.Body.Bytes(), photo) || file.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("admin logo file: %d %v", file.Code, file.Header())
	}

	before := e.storedFiles()
	approved := e.approve(admin, app.ID)
	if approved.Created.Profile == nil || approved.Created.Profile.Slug != "jacaranda-homes" {
		t.Fatalf("created = %+v", approved.Created)
	}
	// Promoted: a copy under its own key. The document stays where it was.
	if e.storedFiles() != before+1 {
		t.Fatalf("%d stored files after approval, want %d (the document and its public copy)", e.storedFiles(), before+1)
	}
	logo := e.profileLogo("agencies", "jacaranda-homes")
	if logo == nil || logo.Width == nil || *logo.Width != 300 || logo.Height == nil || *logo.Height != 200 ||
		!strings.HasPrefix(logo.URL, "/v1/directory/agencies/jacaranda-homes/logo?v=") {
		t.Fatalf("profile logo = %+v", logo)
	}
	served := e.get(logo.URL)
	if served.Code != http.StatusOK || served.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(served.Body.Bytes(), photo) ||
		served.Header().Get("Cache-Control") != logoCacheControlVersioned {
		t.Fatalf("served logo: %d %v", served.Code, served.Header())
	}
	// The new owner manages the profile and its logo.
	owner := e.loginCookie("logo.agency@example.com", registerBody("x")["password"].(string))
	if got := e.myProfiles(withCookie(owner)); len(got) != 1 || got[0].Kind != "agencies" || got[0].Slug != "jacaranda-homes" || !got[0].Verified ||
		got[0].Logo == nil || got[0].Logo.URL != logo.URL {
		t.Fatalf("my profiles = %+v", got)
	}

	// The private documents stay private: only their owner and a reviewer
	// reach them, signed in; the public logo is its own object.
	for _, d := range detail.Documents {
		for _, path := range []string{
			"/v1/me/onboarding/" + role + "/documents/" + d.ID + "/file",
			"/v1/admin/onboarding/" + app.ID + "/documents/" + d.ID + "/file",
		} {
			wantError(t, e.get(path), http.StatusUnauthorized, httpx.CodeUnauthenticated)
		}
		for _, guess := range []string{"/v1/directory/agencies/jacaranda-homes/" + d.ID, "/v1/directory/agencies/jacaranda-homes/logo/" + d.ID,
			"/v1/directory/agencies/jacaranda-homes/documents/" + d.ID + "/file"} {
			if rec := e.get(guess); rec.Code != http.StatusNotFound {
				t.Fatalf("%s = %d", guess, rec.Code)
			}
		}
	}
	// Removing the public logo leaves the private document in place.
	wantStatus(t, e.do(http.MethodDelete, "/v1/me/profiles/agencies/jacaranda-homes/logo", nil, browser(owner)), http.StatusNoContent)
	if still := e.get("/v1/admin/onboarding/"+app.ID+"/documents/"+logoDoc.ID+"/file", withCookie(admin)); still.Code != http.StatusOK {
		t.Fatalf("the private logo document went with the public logo: %d", still.Code)
	}

	// An agent's photo is promoted the same way.
	agent, agentApp := e.apply("logo.agent@example.com", "SALES_AGENT", "")
	_ = agent
	// (apply submitted it without a photo: such an agent has none.)
	out := e.approve(admin, agentApp.ID)
	if out.Created.Profile == nil || e.profileLogo("agents", out.Created.Profile.Slug) != nil {
		t.Fatalf("an agent approved without a photo: %+v", out.Created)
	}
}

func TestAgentPhotoIsPromoted(t *testing.T) {
	e := newEnv(t)
	const role = "AGENT"
	c := e.selfRegister("photo.agent@example.com", "CONSUMER", role)
	wantStatus(t, e.put("/v1/me/onboarding/"+role, detailsFor(role, ""), browser(c)), http.StatusOK)
	for _, doc := range requiredDocuments(role) {
		wantStatus(t, e.upload(c, role, doc, "scan.pdf", testPDF()), http.StatusCreated)
	}
	wantStatus(t, e.upload(c, role, "LOGO", "me.png", logoPNG(256, 320)), http.StatusCreated)
	rec := e.post("/v1/me/onboarding/"+role+"/submit", nil, browser(c))
	wantStatus(t, rec, http.StatusOK)
	out := e.approve(e.admin(), decode[oneApplication](t, rec).Application.ID)
	logo := e.profileLogo("agents", out.Created.Profile.Slug)
	if logo == nil || *logo.Width != 256 || *logo.Height != 320 {
		t.Fatalf("agent photo = %+v", logo)
	}
	// The agency page lists the agent with the photo.
	agency := e.get("/v1/directory/agencies/sample-estates").Body.String()
	if !strings.Contains(agency, `"slug":"`+out.Created.Profile.Slug+`"`) || !strings.Contains(agency, logo.URL) {
		t.Fatalf("the agency's agents do not show the photo: %s", agency)
	}
}

// ---------------------------------------------------------------------------
// The owner changes the logo
// ---------------------------------------------------------------------------

func TestOwnerChangesLogo(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	owner, agent, otherOwner := c.as(devseed.EstatesOwnerEmail), c.as(identity.SeedAgentEmail), c.as(devseed.RealtyAgentEmail)

	// What each person may manage.
	if got := addresses(e.myProfiles(owner)); got != "agencies/sample-estates,agents/rudo-moyo" {
		t.Fatalf("the agency principal manages %s", got)
	}
	if got := addresses(e.myProfiles(agent)); got != "agents/tendai-ncube" {
		t.Fatalf("an agent manages %s", got)
	}
	if got := addresses(e.myProfiles(otherOwner)); got != "agencies/sample-realty,agents/tatenda-mutasa" {
		t.Fatalf("another agency's principal manages %s", got)
	}
	if got := e.myProfiles(c.as(identity.SeedConsumerEmail)); len(got) != 0 {
		t.Fatalf("a customer manages %+v", got)
	}
	wantError(t, e.get("/v1/me/profiles"), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	before := e.profileLogo("agencies", "sample-estates")
	files := e.storedFiles()

	// Who may NOT change the agency's logo.
	replacement := logoPNG(320, 240)
	wantError(t, e.putLogo("agencies", "sample-estates", replacement), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.putLogo("agencies", "sample-estates", replacement, agent), http.StatusForbidden, httpx.CodeForbidden) // AGENT of the same agency
	wantError(t, e.putLogo("agencies", "sample-estates", replacement, otherOwner), http.StatusNotFound, directory.CodeProfileNotFound)
	wantError(t, e.putLogo("agencies", "sample-estates", replacement, c.as(identity.SeedConsumerEmail)), http.StatusNotFound, directory.CodeProfileNotFound)
	wantError(t, e.putLogo("agencies", "no-such-agency", replacement, owner), http.StatusNotFound, directory.CodeProfileNotFound)
	// Nor another agent's photo, not even the principal of the same agency.
	wantError(t, e.putLogo("agents", "tendai-ncube", replacement, owner), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.putLogo("agents", "tendai-ncube", replacement, otherOwner), http.StatusNotFound, directory.CodeProfileNotFound)
	for _, who := range []reqOpt{agent, otherOwner} {
		rec := e.do(http.MethodDelete, "/v1/me/profiles/agencies/sample-estates/logo", nil, who)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Fatalf("DELETE by someone else = %d", rec.Code)
		}
	}
	// The cookie session needs the web app's Origin (CSRF), as everywhere.
	cookie := e.loginCookie(devseed.EstatesOwnerEmail, seedPassword)
	wantError(t, e.putLogo("agencies", "sample-estates", replacement, withCookie(cookie)), http.StatusForbidden, httpx.CodeCSRFOrigin)
	wantError(t, e.do(http.MethodDelete, "/v1/me/profiles/agencies/sample-estates/logo", nil, withCookie(cookie), withOrigin("https://evil.example")),
		http.StatusForbidden, httpx.CodeCSRFOrigin)
	// The same rules as at onboarding.
	wantError(t, e.putLogo("agencies", "sample-estates", []byte(testSVG), owner), http.StatusUnsupportedMediaType, directory.CodeLogoUnsupported)
	wantError(t, e.putLogo("agencies", "sample-estates", logoGIF(200, 200), owner), http.StatusUnsupportedMediaType, directory.CodeLogoUnsupported)
	wantError(t, e.putLogo("agencies", "sample-estates", animatedPNG(), owner), http.StatusUnsupportedMediaType, directory.CodeLogoUnsupported)
	wantError(t, e.putLogo("agencies", "sample-estates", noisyPNG(1024), owner), http.StatusRequestEntityTooLarge, directory.CodeLogoTooLarge)
	if env := wantError(t, e.putLogo("agencies", "sample-estates", logoPNG(100, 100), owner), http.StatusBadRequest, httpx.CodeValidationFailed); !strings.Contains(env.Error.Fields["file"], "128x128") {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
	wantError(t, e.putLogo("agencies", "sample-estates", nil, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	if after := e.profileLogo("agencies", "sample-estates"); after.URL != before.URL || e.storedFiles() != files {
		t.Fatalf("a refused change changed the logo: %+v", after)
	}

	// The principal replaces it (a browser: cookie + Origin).
	rec := e.putLogo("agencies", "sample-estates", replacement, browser(cookie))
	wantStatus(t, rec, http.StatusOK)
	changed := decode[struct {
		Profile managedBody `json:"profile"`
	}](t, rec).Profile
	if changed.Logo == nil || changed.Logo.URL == before.URL || *changed.Logo.Width != 320 || *changed.Logo.Height != 240 || !changed.Verified ||
		!strings.HasPrefix(changed.Logo.URL, "/v1/directory/agencies/sample-estates/logo?v=") {
		t.Fatalf("changed = %+v (before %+v)", changed, before)
	}
	if now := e.profileLogo("agencies", "sample-estates"); *now.Width != 320 || now.URL != changed.Logo.URL {
		t.Fatalf("public profile logo = %+v", now)
	}
	served := e.get(changed.Logo.URL)
	if served.Code != http.StatusOK || !bytes.Equal(served.Body.Bytes(), replacement) {
		t.Fatalf("served logo = %d", served.Code)
	}
	// The old image is gone from storage, and the old URL serves the new one.
	if e.storedFiles() != files {
		t.Fatalf("%d stored files, want %d: the replaced image was kept", e.storedFiles(), files)
	}
	if old := e.get(before.URL); old.Code != http.StatusOK || !bytes.Equal(old.Body.Bytes(), replacement) {
		t.Fatal("the old URL does not serve the current logo")
	} else if old.Header().Get("Cache-Control") != logoCacheControl || served.Header().Get("Cache-Control") != logoCacheControlVersioned {
		// The old URL now shows another image: it must not be kept for a year.
		t.Fatalf("Cache-Control: old URL %q, new URL %q", old.Header().Get("Cache-Control"), served.Header().Get("Cache-Control"))
	}

	// A logo larger than it is ever shown is scaled down when it is stored:
	// the profile says the size of the image that is served.
	rec = e.putLogo("agencies", "sample-estates", logoPNG(1600, 800), owner)
	wantStatus(t, rec, http.StatusOK)
	large := decode[struct {
		Profile managedBody `json:"profile"`
	}](t, rec).Profile.Logo
	if large == nil || *large.Width != directory.LogoStoredPixels || *large.Height != directory.LogoStoredPixels/2 {
		t.Fatalf("large logo = %+v", large)
	}
	shrunk := e.get(large.URL)
	if img, err := directory.CheckLogo(shrunk.Body.Bytes()); shrunk.Code != http.StatusOK || err != nil || img.Width != 512 || img.Height != 256 ||
		shrunk.Header().Get("ETag") != `"`+img.Checksum+`"` || shrunk.Header().Get("Content-Length") != fmt.Sprint(shrunk.Body.Len()) ||
		!strings.HasSuffix(large.URL, "?v="+img.Checksum[:listings.LogoVersionLength]) || shrunk.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("served large logo: %d %+v %v %v", shrunk.Code, img, err, shrunk.Header())
	}
	wantStatus(t, e.putLogo("agencies", "sample-estates", replacement, owner), http.StatusOK)
	if e.storedFiles() != files {
		t.Fatalf("%d stored files, want %d", e.storedFiles(), files)
	}
	// The listings of the agency show the new logo at once.
	if l := e.listing("four-bedroom-family-home-borrowdale"); l.Advertiser.Display.Logo == nil || l.Advertiser.Display.Logo.URL != changed.Logo.URL ||
		l.Advertiser.Agency.Logo.URL != changed.Logo.URL {
		t.Fatalf("listing logo = %+v", l.Advertiser.Display.Logo)
	}
	if !strings.Contains(e.logs.String(), `"event":"profile.logo_changed"`) {
		t.Fatal("no audit line for the logo change")
	}

	// A MANAGER may too; STAFF may not.
	org := organisationOf(t, devseed.EstatesOwnerEmail)
	wantStatus(t, e.putLogo("agencies", "sample-estates", logoJPEG(200, 200), c.as(c.member("logo.manager@example.com", org, "MANAGER"))), http.StatusOK)
	wantError(t, e.putLogo("agencies", "sample-estates", replacement, c.as(c.member("logo.staff2@example.com", org, "STAFF"))), http.StatusForbidden, httpx.CodeForbidden)

	// The agent changes their own photo: it appears on their listings, next
	// to the agency's logo.
	wantStatus(t, e.putLogo("agents", "tendai-ncube", logoPNG(200, 260), agent), http.StatusOK)
	l := e.listing("four-bedroom-family-home-borrowdale")
	if l.Advertiser.Agent.Logo == nil || *l.Advertiser.Agent.Logo.Height != 260 || l.Advertiser.AgentDisplay.Logo == nil ||
		l.Advertiser.Display.Logo == nil || !strings.Contains(l.Advertiser.Display.Logo.URL, "/agencies/sample-estates/") {
		t.Fatalf("advertiser = %+v", l.Advertiser)
	}

	// Delete: the profile says null, the route answers 404, the listing falls
	// back to the agent's photo; deleting again is still 204.
	for range 2 {
		wantStatus(t, e.do(http.MethodDelete, "/v1/me/profiles/agencies/sample-estates/logo", nil, owner), http.StatusNoContent)
	}
	if e.profileLogo("agencies", "sample-estates") != nil {
		t.Fatal("the logo is still on the profile")
	}
	wantError(t, e.get("/v1/directory/agencies/sample-estates/logo"), http.StatusNotFound, directory.CodeLogoNotFound)
	for _, p := range e.myProfiles(owner) {
		if p.Logo != nil {
			t.Fatalf("my profile %s still has a logo", p.Slug)
		}
	}
	l = e.listing("four-bedroom-family-home-borrowdale")
	if l.Advertiser.Agency.Logo != nil || l.Advertiser.Display.Logo == nil || !strings.Contains(l.Advertiser.Display.Logo.URL, "/agents/tendai-ncube/") {
		t.Fatalf("fallback logo = %+v", l.Advertiser.Display.Logo)
	}
	// ...and with no photo either, null: the client shows initials.
	wantStatus(t, e.do(http.MethodDelete, "/v1/me/profiles/agents/tendai-ncube/logo", nil, agent), http.StatusNoContent)
	if l = e.listing("four-bedroom-family-home-borrowdale"); l.Advertiser.Display.Logo != nil || l.Advertiser.AgentDisplay.Logo != nil {
		t.Fatalf("logo after both were removed = %+v", l.Advertiser.Display.Logo)
	}
}

func TestLogoUploadRateLimit(t *testing.T) {
	e := newEnv(t)
	owner := withBearer(e.loginBearer(devseed.EstatesOwnerEmail, seedPassword))
	limited := false
	for range UploadRequestsBurst + 2 {
		rec := e.do(http.MethodDelete, "/v1/me/profiles/agencies/sample-estates/logo", nil, owner, withIP("203.0.113.77"))
		if rec.Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("logo changes are not rate limited")
	}
}

// ---------------------------------------------------------------------------
// advertiser.display
// ---------------------------------------------------------------------------

func TestAdvertiserDisplay(t *testing.T) {
	e := newEnv(t)
	text := func(s *string) string {
		if s == nil {
			return "<null>"
		}
		return *s
	}
	link := func(p *listings.ProfileLink) string {
		if p == nil {
			return "<null>"
		}
		return p.Kind + "/" + p.Slug
	}
	show := func(a listings.Advertiser) string {
		d := a.Display
		out := fmt.Sprintf("%s|%s|%s|%s|verified=%v|logo=%v|%s", a.Type, d.Name, text(d.SecondaryName), d.RoleLabel, d.Verified, d.Logo != nil, link(d.Profile))
		if ad := a.AgentDisplay; ad != nil {
			out += fmt.Sprintf(" / %s|%s|verified=%v|logo=%v|%s", ad.Name, ad.RoleLabel, ad.Verified, ad.Logo != nil, link(ad.Profile))
		}
		return out
	}

	// A law firm's listing and a direct owner's rental: not in the sample data.
	lawFirm := uuid.Nil
	for _, p := range devseed.Profiles() {
		if p.Key() == "solicitors/sample-law-chambers" {
			lawFirm = p.Organisation.ID
		}
	}
	base, err := e.listings.BySlug(t.Context(), "office-suite-eastlea")
	if err != nil || lawFirm == uuid.Nil {
		t.Fatal(err)
	}
	legal := base
	legal.ID, legal.Slug, legal.Reference, legal.OrganisationID, legal.AgentUserID = uuid.New(), "chambers-office-harare", "RP-ZW-19001", &lawFirm, nil
	e.listings.Put(legal)
	seller := devseed.SellerID
	rental := base
	rental.ID, rental.Slug, rental.Reference = uuid.New(), "owner-let-cottage", "RP-ZW-19002"
	rental.OrganisationID, rental.AgentUserID, rental.SellerUserID = nil, nil, &seller
	e.listings.Put(rental)

	want := map[string]string{
		// A business and its assigned agent.
		"four-bedroom-family-home-borrowdale": "agency|Sample Estates|Tendai Ncube|Real Estate Agency|verified=true|logo=true|agencies/sample-estates" +
			" / Tendai Ncube|Estate Agent|verified=true|logo=false|agents/tendai-ncube",
		// A business only (the lister has no agent profile).
		"three-bedroom-terraced-house-kensington": "agency|Sample Homes London|<null>|Real Estate Agency|verified=true|logo=true|agencies/sample-homes-london",
		// A host's short stay.
		"safari-lodge-victoria-falls": "agency|Sample Falls Stays|<null>|Host|verified=true|logo=true|hosts/sample-falls-stays",
		// A law firm's listing.
		"chambers-office-harare": "agency|Sample Law Chambers|<null>|Law Firm|verified=true|logo=true|solicitors/sample-law-chambers",
		// A direct owner: a sale, then a rental. Their name, no logo, no page.
		"five-bedroom-house-victoria-falls": "private_seller|Grace Mhlanga|<null>|Property Seller (Vendor)|verified=true|logo=false|<null>",
		"owner-let-cottage":                 "private_seller|Grace Mhlanga|<null>|Landlord|verified=true|logo=false|<null>",
	}
	// Whatever the sample agency without a logo lists, it shows no logo.
	realty := 0
	for _, l := range e.search("?page_size=48").Items {
		if l.Advertiser.Agency != nil && l.Advertiser.Agency.Slug == "sample-realty" {
			realty++
			if l.Advertiser.Display.Logo != nil || l.Advertiser.Agency.Logo != nil || !l.Advertiser.Display.Verified || l.Advertiser.Display.Name != "Sample Realty" ||
				text(l.Advertiser.Display.SecondaryName) != "Tatenda Mutasa" {
				t.Fatalf("%s: %s", l.Slug, show(l.Advertiser))
			}
		}
	}
	if realty != 4 {
		t.Fatalf("%d listings of the agency without a logo", realty)
	}

	if rental.Deal != "rent" {
		t.Fatalf("test setup: the office suite is %q", rental.Deal)
	}
	found := map[string]listings.Advertiser{}
	for _, l := range e.search("?page_size=48").Items {
		found[l.Slug] = l.Advertiser
	}
	for slug, w := range want {
		detail := e.listing(slug).Advertiser
		if got := show(detail); got != w {
			t.Errorf("%s detail:\n got %s\nwant %s", slug, got, w)
		}
		// The same block in search results as on the detail page.
		inSearch, ok := found[slug]
		if !ok || show(inSearch) != w {
			t.Errorf("%s in search results: %s", slug, show(inSearch))
		}
		// name and verified repeat display's, for clients that read them.
		if detail.Name != detail.Display.Name || detail.Verified != detail.Display.Verified {
			t.Errorf("%s: name/verified %q/%v differ from display", slug, detail.Name, detail.Verified)
		}
	}

	// The JSON shape, key by key.
	raw := e.get("/v1/listings/four-bedroom-family-home-borrowdale").Body.String()
	logoURL := e.profileLogo("agencies", "sample-estates").URL
	for _, part := range []string{
		`"advertiser":{"type":"agency","name":"Sample Estates","verified":true,` +
			`"agency":{"kind":"agencies","slug":"sample-estates","name":"Sample Estates","verified":true,"logo":{"url":"` + logoURL + `","width":256,"height":256}},` +
			`"agent":{"kind":"agents","slug":"tendai-ncube","name":"Tendai Ncube","verified":true,"logo":null},`,
		`"display":{"name":"Sample Estates","secondary_name":"Tendai Ncube","role_label":"Real Estate Agency","logo":{"url":"` + logoURL + `","width":256,"height":256},` +
			`"verified":true,"profile":{"kind":"agencies","slug":"sample-estates"}},`,
		`"agent_display":{"name":"Tendai Ncube","role_label":"Estate Agent","verified":true,"logo":null,"profile":{"kind":"agents","slug":"tendai-ncube"}}}`,
		`"verification":{"advertiser_verified":true,"advertiser_verified_at":"`,
	} {
		if !strings.Contains(raw, part) {
			t.Fatalf("the listing lacks %s\n%s", part, raw)
		}
	}
	if strings.Contains(raw, "specialism") || strings.Contains(raw, "organisation_kind") || strings.Contains(raw, "verified_at\":null,\"listing_reviewed\":false") {
		t.Fatalf("internal fields in the listing: %s", raw)
	}

	// An agent's specialism is their role label, from the reference data.
	tendai, err := e.directory.Find(t.Context(), "agents", "tendai-ncube")
	if err != nil {
		t.Fatal(err)
	}
	for specialism, label := range map[string]string{"SALES": "Sales Agent", "LETTINGS": "Lettings Agent", "": "Estate Agent"} {
		tendai.Specialism = specialism
		e.directory.Put(tendai)
		if got := e.listing("four-bedroom-family-home-borrowdale").Advertiser.AgentDisplay.RoleLabel; got != label {
			t.Fatalf("specialism %q: role label %q, want %q", specialism, got, label)
		}
	}
	// The labels are reference data: renaming the row renames the label.
	e.reference.Put(reference.SetOrganisationKinds, reference.Item{Code: "AGENCY", Label: "Estate Agency", SortOrder: 10, Active: true})
	if got := e.listing("four-bedroom-family-home-borrowdale").Advertiser.Display.RoleLabel; got != "Estate Agency" {
		t.Fatalf("role label after the reference row changed = %q", got)
	}

	// An agent who is no longer verified: still named, not verified, no page,
	// and the listing stays public because the agency is what gates it.
	tendai.Verified = false
	e.directory.Put(tendai)
	a := e.listing("four-bedroom-family-home-borrowdale").Advertiser
	if !a.Verified || !a.Display.Verified || a.AgentDisplay == nil || a.AgentDisplay.Verified || a.AgentDisplay.Profile != nil || a.Agent.Verified ||
		text(a.Display.SecondaryName) != "Tendai Ncube" {
		t.Fatalf("unverified agent: %s", show(a))
	}
}
