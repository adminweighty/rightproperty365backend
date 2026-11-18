package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// YouTube property tours (API 0.28.0, BR-013, FR-011).

// videoContent is the video part of a listing's content.
type videoContent struct {
	YouTubeURL          *string `json:"youtube_url"`
	YouTubeVideoID      *string `json:"youtube_video_id"`
	YouTubeStartSeconds *int    `json:"youtube_start_seconds"`
}

type videoView struct {
	ID             string `json:"id"`
	Slug           string `json:"slug"`
	Status         string `json:"status"`
	PendingChanges *struct {
		Status string `json:"status"`
	} `json:"pending_changes"`
	Content      videoContent `json:"content"`
	Completeness struct {
		Ready   bool `json:"ready"`
		Missing []struct {
			Field string `json:"field"`
		} `json:"missing"`
	} `json:"completeness"`
	// Reviewer view only.
	ReviewKind *string       `json:"review_kind"`
	Live       *videoContent `json:"live"`
	Changes    []struct {
		Field  string `json:"field"`
		Before any    `json:"before"`
		After  any    `json:"after"`
	} `json:"changes"`
}

func videoViewOf(t *testing.T, rec *httptest.ResponseRecorder, status int) videoView {
	t.Helper()
	wantStatus(t, rec, status)
	return decode[struct {
		Listing videoView `json:"listing"`
	}](t, rec).Listing
}

func wantVideo(t *testing.T, got videoContent, url, id string, start int) {
	t.Helper()
	if id == "" {
		if got.YouTubeURL != nil || got.YouTubeVideoID != nil || got.YouTubeStartSeconds != nil {
			t.Fatalf("want no video, got %v %v %v", got.YouTubeURL, got.YouTubeVideoID, got.YouTubeStartSeconds)
		}
		return
	}
	if got.YouTubeURL == nil || *got.YouTubeURL != url || got.YouTubeVideoID == nil || *got.YouTubeVideoID != id {
		t.Fatalf("video = %v %v, want %s %s", got.YouTubeURL, got.YouTubeVideoID, url, id)
	}
	switch {
	case start == 0 && got.YouTubeStartSeconds != nil:
		t.Fatalf("start = %d, want none", *got.YouTubeStartSeconds)
	case start != 0 && (got.YouTubeStartSeconds == nil || *got.YouTubeStartSeconds != start):
		t.Fatalf("start = %v, want %d", got.YouTubeStartSeconds, start)
	}
}

func TestListingVideoIsSetChangedAndCleared(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	l := e.readyListing(tendai, "House with a video tour")
	path := "/v1/me/listings/" + l.ID

	// No video is never missing.
	v := videoViewOf(t, e.get(path, tendai), http.StatusOK)
	if !v.Completeness.Ready {
		t.Fatalf("a listing without a video must be ready: %+v", v.Completeness)
	}
	wantVideo(t, v.Content, "", "", 0)

	// Set: a share link with a start time; the response has the canonical
	// link, never the one typed.
	v = videoViewOf(t, e.patch(path, obj{"youtube_url": " https://youtu.be/aqz-KE-bpKQ?si=Share123&t=1m30s "}, tendai), http.StatusOK)
	wantVideo(t, v.Content, "https://www.youtube.com/watch?v=aqz-KE-bpKQ&t=90s", "aqz-KE-bpKQ", 90)
	if !v.Completeness.Ready {
		t.Fatalf("still ready with a video: %+v", v.Completeness)
	}
	// Sending the canonical link back keeps the start time.
	v = videoViewOf(t, e.patch(path, obj{"youtube_url": *v.Content.YouTubeURL}, tendai), http.StatusOK)
	wantVideo(t, v.Content, "https://www.youtube.com/watch?v=aqz-KE-bpKQ&t=90s", "aqz-KE-bpKQ", 90)
	// Another field leaves the video alone.
	v = videoViewOf(t, e.patch(path, obj{"title": "House with a video tour, renamed"}, tendai), http.StatusOK)
	wantVideo(t, v.Content, "https://www.youtube.com/watch?v=aqz-KE-bpKQ&t=90s", "aqz-KE-bpKQ", 90)

	// Change: a Shorts link without a start time.
	v = videoViewOf(t, e.patch(path, obj{"youtube_url": "https://www.youtube.com/shorts/jNQXAC9IVRw"}, tendai), http.StatusOK)
	wantVideo(t, v.Content, "https://www.youtube.com/watch?v=jNQXAC9IVRw", "jNQXAC9IVRw", 0)

	// Invalid: a field error on youtube_url, and nothing changes.
	for _, bad := range []any{
		`<iframe width="560" height="315" src="https://www.youtube.com/embed/aqz-KE-bpKQ"></iframe>`,
		"https://youtube.com.evil.test/watch?v=aqz-KE-bpKQ",
		"https://youtube.com@evil.test/watch?v=aqz-KE-bpKQ",
		"https://www.youtube.com/playlist?list=PLabcdefghijklmnop",
		"https://www.youtube.com/@sample-estates",
		"javascript:alert(1)",
		"https://www.youtube.com/watch?v=aqz-KE-bpK",
		42,
	} {
		env := wantError(t, e.patch(path, obj{"youtube_url": bad}, tendai), http.StatusBadRequest, httpx.CodeValidationFailed)
		want := listings.MsgNotYouTubeVideo
		if _, isText := bad.(string); !isText {
			want = "must be a string or null"
		}
		if env.Error.Fields["youtube_url"] != want {
			t.Fatalf("%v: fields = %v", bad, env.Error.Fields)
		}
	}
	v = videoViewOf(t, e.get(path, tendai), http.StatusOK)
	wantVideo(t, v.Content, "https://www.youtube.com/watch?v=jNQXAC9IVRw", "jNQXAC9IVRw", 0)

	// Clear: null and "" both remove it.
	v = videoViewOf(t, e.patch(path, obj{"youtube_url": nil}, tendai), http.StatusOK)
	wantVideo(t, v.Content, "", "", 0)
	videoViewOf(t, e.patch(path, obj{"youtube_url": "https://youtu.be/jNQXAC9IVRw"}, tendai), http.StatusOK)
	v = videoViewOf(t, e.patch(path, obj{"youtube_url": ""}, tendai), http.StatusOK)
	wantVideo(t, v.Content, "", "", 0)
}

// A video on a published listing is a content change: it waits for review,
// the public sees the listing without it until the approval, and the
// reviewer sees the video id and its link.
func TestPublishedListingVideoGoesThroughReview(t *testing.T) {
	e := newEnv(t)
	tendai := newCast(e).as(identity.SeedAgentEmail)
	admin := e.admin()
	id := propertyrequests.SeedListingID(1).String()
	path := "/v1/me/listings/" + id

	v := videoViewOf(t, e.patch(path, obj{"youtube_url": "https://www.youtube.com/watch?v=aqz-KE-bpKQ&t=45"}, tendai), http.StatusOK)
	if v.Status != "PUBLISHED" || v.PendingChanges == nil || v.PendingChanges.Status != "EDITING" {
		t.Fatalf("a video on a published listing must open its pending changes: %+v", v)
	}
	wantVideo(t, v.Content, "https://www.youtube.com/watch?v=aqz-KE-bpKQ&t=45s", "aqz-KE-bpKQ", 45)
	public := decode[struct{ Listing listings.Listing }](t, e.get("/v1/listings/"+v.Slug)).Listing
	if public.Video != nil || public.HasVideo {
		t.Fatalf("an unreviewed video is public: %+v", public.Video)
	}

	videoViewOf(t, e.post(path+"/submit", nil, tendai), http.StatusOK)
	r := videoViewOf(t, e.get("/v1/admin/listings/"+id, browser(admin)), http.StatusOK)
	if r.ReviewKind == nil || *r.ReviewKind != "EDIT" || r.Live == nil {
		t.Fatalf("review: %+v", r)
	}
	wantVideo(t, r.Content, "https://www.youtube.com/watch?v=aqz-KE-bpKQ&t=45s", "aqz-KE-bpKQ", 45)
	wantVideo(t, *r.Live, "", "", 0)
	found := false
	for _, c := range r.Changes {
		if c.Field == "youtube_url" {
			found = c.Before == nil && c.After == "https://www.youtube.com/watch?v=aqz-KE-bpKQ&t=45s"
		}
	}
	if !found {
		t.Fatalf("the diff must show the new video: %+v", r.Changes)
	}

	videoViewOf(t, e.post("/v1/admin/listings/"+id+"/approve", nil, browser(admin)), http.StatusOK)
	public = decode[struct{ Listing listings.Listing }](t, e.get("/v1/listings/"+v.Slug)).Listing
	if !public.HasVideo || public.Video == nil || public.Video.ID != "aqz-KE-bpKQ" || public.Video.StartSeconds == nil || *public.Video.StartSeconds != 45 ||
		public.Video.EmbedURL != "https://www.youtube-nocookie.com/embed/aqz-KE-bpKQ?rel=0&modestbranding=1&start=45" {
		t.Fatalf("approved video: %v %+v", public.HasVideo, public.Video)
	}
}

// The public listing carries the video object and the search cards
// has_video (seed: the London sample has a video starting at 30 seconds).
func TestPublicListingVideo(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/listings/three-bedroom-terraced-house-kensington")
	wantStatus(t, rec, http.StatusOK)
	l := decode[struct{ Listing listings.Listing }](t, rec).Listing
	v := l.Video
	if !l.HasVideo || v == nil || v.Provider != "YOUTUBE" || v.ID != devseed.SampleYouTubeVideoID || v.StartSeconds == nil || *v.StartSeconds != 30 {
		t.Fatalf("video: %v %+v", l.HasVideo, v)
	}
	if v.EmbedURL != "https://www.youtube-nocookie.com/embed/"+v.ID+"?rel=0&modestbranding=1&start=30" ||
		v.WatchURL != "https://www.youtube.com/watch?v="+v.ID || v.ThumbnailURL != "https://i.ytimg.com/vi/"+v.ID+"/hqdefault.jpg" {
		t.Fatalf("video addresses: %+v", v)
	}
	if !strings.HasPrefix(v.EmbedURL, "https://www.youtube-nocookie.com/") || !strings.Contains(v.EmbedURL, "rel=0") {
		t.Fatalf("embed must be privacy-enhanced without related videos: %s", v.EmbedURL)
	}
	// A listing without a video says so explicitly.
	if !strings.Contains(e.get("/v1/listings/two-bedroom-apartment-northern-quarter").Body.String(), `"video":null`) {
		t.Fatal("a listing without a video must have video: null")
	}

	page := decode[listings.Page](t, e.get("/v1/listings?country=GB&page_size=48"))
	seen := map[string]bool{}
	for _, it := range page.Items {
		seen[it.Slug] = it.HasVideo
	}
	if v, ok := seen["three-bedroom-terraced-house-kensington"]; !ok || !v {
		t.Fatalf("the London sample must have has_video: %v", seen)
	}
	if v, ok := seen["two-bedroom-apartment-northern-quarter"]; !ok || v {
		t.Fatalf("the Manchester sample must not have has_video: %v", seen)
	}
	if !strings.Contains(e.get("/v1/listings?country=GB").Body.String(), `"has_video":false`) {
		t.Fatal("cards must carry has_video")
	}
}
