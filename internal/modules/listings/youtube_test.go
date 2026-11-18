package listings

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseYouTubeURL(t *testing.T) {
	const id = "dQw4w9WgXcQ"
	ok := []struct {
		name, in string
		start    int // 0: no start time
	}{
		{"watch", "https://www.youtube.com/watch?v=" + id, 0},
		{"watch without www", "https://youtube.com/watch?v=" + id, 0},
		{"watch over http", "http://www.youtube.com/watch?v=" + id, 0},
		{"upper-case scheme and host", "HTTPS://WWW.YouTube.COM/watch?v=" + id, 0},
		{"surrounding spaces", "  https://www.youtube.com/watch?v=" + id + "\n", 0},
		{"watch with other parameters", "https://www.youtube.com/watch?feature=share&v=" + id + "&ab_channel=X", 0},
		{"watch in a playlist", "https://www.youtube.com/watch?v=" + id + "&list=PLabcdefghijkl&index=2", 0},
		{"watch with a fragment", "https://www.youtube.com/watch?v=" + id + "#comments", 0},
		{"mobile", "https://m.youtube.com/watch?v=" + id, 0},
		{"music", "https://music.youtube.com/watch?v=" + id, 0},
		{"share link", "https://youtu.be/" + id, 0},
		{"share link with si", "https://youtu.be/" + id + "?si=AbCdEf123", 0},
		{"share link with a trailing slash", "https://youtu.be/" + id + "/", 0},
		{"shorts", "https://www.youtube.com/shorts/" + id, 0},
		{"shorts with feature", "https://youtube.com/shorts/" + id + "?feature=share", 0},
		{"embed", "https://www.youtube.com/embed/" + id, 0},
		{"live", "https://www.youtube.com/live/" + id + "?si=x", 0},
		{"old /v/", "https://www.youtube.com/v/" + id, 0},
		{"nocookie embed", "https://www.youtube-nocookie.com/embed/" + id + "?rel=0", 0},
		{"nocookie without www", "https://youtube-nocookie.com/embed/" + id, 0},
		{"id with - and _", "https://youtu.be/a-b_c-d_e-f", 0},

		{"t in seconds", "https://youtu.be/" + id + "?t=90", 90},
		{"t with s", "https://www.youtube.com/watch?v=" + id + "&t=90s", 90},
		{"t as minutes and seconds", "https://www.youtube.com/watch?v=" + id + "&t=1m30s", 90},
		{"t as hours, minutes and seconds", "https://www.youtube.com/watch?v=" + id + "&t=1h2m3s", 3723},
		{"t as hours only", "https://www.youtube.com/watch?v=" + id + "&t=1h", 3600},
		{"t as minutes only", "https://www.youtube.com/watch?v=" + id + "&t=2m", 120},
		{"start on embed", "https://www.youtube.com/embed/" + id + "?start=45", 45},
		{"t=0 is the beginning", "https://youtu.be/" + id + "?t=0", 0},
		{"t wins over start", "https://www.youtube.com/watch?v=" + id + "&start=5&t=10", 10},
	}
	for _, c := range ok {
		t.Run("accepts "+c.name, func(t *testing.T) {
			gotID, start, err := ParseYouTubeURL(c.in)
			want := "dQw4w9WgXcQ"
			if strings.Contains(c.in, "a-b_c-d_e-f") {
				want = "a-b_c-d_e-f"
			}
			if err != nil || gotID != want {
				t.Fatalf("ParseYouTubeURL(%q) = %q, %v, %v", c.in, gotID, start, err)
			}
			switch {
			case c.start == 0 && start != nil:
				t.Fatalf("ParseYouTubeURL(%q): start %d, want none", c.in, *start)
			case c.start != 0 && (start == nil || *start != c.start):
				t.Fatalf("ParseYouTubeURL(%q): start %v, want %d", c.in, start, c.start)
			}
		})
	}

	bad := []struct{ name, in string }{
		{"empty", ""},
		{"spaces", "   "},
		{"no scheme", "www.youtube.com/watch?v=" + id},
		{"scheme-relative", "//www.youtube.com/watch?v=" + id},
		{"ftp", "ftp://www.youtube.com/watch?v=" + id},
		{"javascript", "javascript:alert(1)//https://www.youtube.com/watch?v=" + id},
		{"javascript upper case", "JavaScript:alert('https://youtu.be/" + id + "')"},
		{"data", "data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg=="},
		{"iframe markup", `<iframe width="560" height="315" src="https://www.youtube.com/embed/` + id + `"></iframe>`},
		{"html", "<a href=\"https://youtu.be/" + id + "\">tour</a>"},
		{"script in the query", "https://www.youtube.com/watch?v=" + id + "&x=<script>"},
		{"a bare id", id},
		{"other host", "https://vimeo.com/123456789"},
		{"look-alike suffix", "https://youtube.com.evil.test/watch?v=" + id},
		{"look-alike prefix", "https://evilyoutube.com/watch?v=" + id},
		{"look-alike subdomain", "https://www.youtube.co/watch?v=" + id},
		{"other subdomain", "https://studio.youtube.com/watch?v=" + id},
		{"youtu.be look-alike", "https://youtu.be.evil.test/" + id},
		{"trailing dot", "https://www.youtube.com./watch?v=" + id},
		{"userinfo trick", "https://youtube.com@evil.test/watch?v=" + id},
		{"userinfo on youtube", "https://user:pass@www.youtube.com/watch?v=" + id},
		{"fragment trick", "https://evil.test#@www.youtube.com/watch?v=" + id},
		{"backslash trick", `https://evil.test\@www.youtube.com/watch?v=` + id},
		{"percent-encoded host", "https://www.youtube%2Ecom/watch?v=" + id},
		{"percent-encoded host entirely", "https://%77%77%77.youtube.com/watch?v=" + id},
		{"percent-encoded path", "https://www.youtube.com/%77atch?v=" + id},
		{"port", "https://www.youtube.com:8443/watch?v=" + id},
		{"ip host", "https://142.250.0.1/watch?v=" + id},
		{"playlist only", "https://www.youtube.com/playlist?list=PLabcdefghijklmnop"},
		{"watch with a list only", "https://www.youtube.com/watch?list=PLabcdefghijklmnop"},
		{"channel", "https://www.youtube.com/channel/UCabcdefghijklmnopqrstuv"},
		{"channel with an 11-character segment", "https://www.youtube.com/c/abcdefghijk"},
		{"user", "https://www.youtube.com/user/abcdefghijk"},
		{"handle", "https://www.youtube.com/@abcdefghijk"},
		{"handle videos", "https://www.youtube.com/@agency/videos"},
		{"home page", "https://www.youtube.com/"},
		{"results", "https://www.youtube.com/results?search_query=house+tour"},
		{"missing id", "https://www.youtube.com/watch?v="},
		{"missing v", "https://www.youtube.com/watch"},
		{"empty share link", "https://youtu.be/"},
		{"two ids", "https://www.youtube.com/watch?v=" + id + "&v=aqz-KE-bpKQ"},
		{"10-character id", "https://www.youtube.com/watch?v=dQw4w9WgXc"},
		{"12-character id", "https://www.youtube.com/watch?v=dQw4w9WgXcQQ"},
		{"10-character share id", "https://youtu.be/dQw4w9WgXc"},
		{"12-character shorts id", "https://www.youtube.com/shorts/dQw4w9WgXcQQ"},
		{"bad id character", "https://youtu.be/dQw4w9WgX.Q"},
		{"extra share segment", "https://youtu.be/" + id + "/extra"},
		{"extra embed segment", "https://www.youtube.com/embed/" + id + "/x"},
		{"watch with a trailing slash", "https://www.youtube.com/watch/?v=" + id},
		{"upper-case path", "https://www.youtube.com/Watch?v=" + id},
		{"dot segment", "https://www.youtube.com/embed/../watch?v=" + id},
		{"double slash", "https://www.youtube.com//embed/" + id},
		{"space inside", "https://www.youtube.com/watch?v=" + id + " https://evil.test"},
		{"control character", "https://www.youtube.com/watch?v=" + id + "\x00"},
		{"unreadable t", "https://youtu.be/" + id + "?t=soon"},
		{"negative t", "https://youtu.be/" + id + "?t=-5"},
		{"t too late", "https://youtu.be/" + id + "?t=100000"},
		{"two t", "https://youtu.be/" + id + "?t=1&t=2"},
		{"far too long", "https://www.youtube.com/watch?v=" + id + "&x=" + strings.Repeat("a", 3000)},
	}
	for _, c := range bad {
		t.Run("refuses "+c.name, func(t *testing.T) {
			if gotID, start, err := ParseYouTubeURL(c.in); !errors.Is(err, ErrNotYouTubeVideo) || gotID != "" || start != nil {
				t.Fatalf("ParseYouTubeURL(%q) = %q, %v, %v; want ErrNotYouTubeVideo", c.in, gotID, start, err)
			}
		})
	}
}

func TestYouTubeAddresses(t *testing.T) {
	start := 90
	if got := YouTubeEmbedURL("dQw4w9WgXcQ", nil); got != "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ?rel=0&modestbranding=1" {
		t.Fatal(got)
	}
	if got := YouTubeEmbedURL("dQw4w9WgXcQ", &start); got != "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ?rel=0&modestbranding=1&start=90" {
		t.Fatal(got)
	}
	if got := YouTubeWatchURL("dQw4w9WgXcQ"); got != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Fatal(got)
	}
	if got := YouTubeThumbnailURL("dQw4w9WgXcQ"); got != "https://i.ytimg.com/vi/dQw4w9WgXcQ/hqdefault.jpg" {
		t.Fatal(got)
	}
	// The editor's link keeps the start time, and parses back to the same.
	link := youTubeEditURL("dQw4w9WgXcQ", &start)
	if id, s, err := ParseYouTubeURL(link); err != nil || id != "dQw4w9WgXcQ" || s == nil || *s != 90 {
		t.Fatalf("%s: %q %v %v", link, id, s, err)
	}
	if videoOf("", nil) != nil || videoOf("short", nil) != nil {
		t.Fatal("no id, no video")
	}
	raw, _ := json.Marshal(videoOf("dQw4w9WgXcQ", nil))
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"provider": "YOUTUBE", "id": "dQw4w9WgXcQ", "start_seconds": nil,
		"embed_url":     "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ?rel=0&modestbranding=1",
		"watch_url":     "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"thumbnail_url": "https://i.ytimg.com/vi/dQw4w9WgXcQ/hqdefault.jpg",
	}
	if len(got) != len(want) {
		t.Fatalf("video JSON: %s", raw)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("video JSON %s = %v, want %v (%s)", k, got[k], v, raw)
		}
	}
}

// The revision content keeps the video, and an older revision without it
// reads as no video.
func TestContentCarriesTheVideo(t *testing.T) {
	start := 12
	r := Record{YouTubeVideoID: "dQw4w9WgXcQ", YouTubeStartSeconds: &start}
	c := ContentOf(r)
	raw, _ := json.Marshal(c)
	var back Content
	if err := json.Unmarshal(raw, &back); err != nil || back.YouTubeVideoID != "dQw4w9WgXcQ" || back.YouTubeStartSeconds == nil || *back.YouTubeStartSeconds != 12 {
		t.Fatalf("round trip: %s %+v %v", raw, back, err)
	}
	var out Record
	back.ApplyTo(&out)
	if out.YouTubeVideoID != "dQw4w9WgXcQ" || out.YouTubeStartSeconds == nil || *out.YouTubeStartSeconds != 12 {
		t.Fatalf("ApplyTo: %+v", out)
	}
	*c.YouTubeStartSeconds = 99
	if *r.YouTubeStartSeconds != 12 {
		t.Fatal("ContentOf must copy the start time")
	}
	var old Content
	if err := json.Unmarshal([]byte(`{"title":"An older revision"}`), &old); err != nil || old.YouTubeVideoID != "" || old.YouTubeStartSeconds != nil {
		t.Fatalf("old revision: %+v %v", old, err)
	}
	// No video is never missing from a submission.
	for _, m := range completeness(Content{}, nil, nil, Geography{}).Missing {
		if strings.Contains(m.Field, "youtube") || strings.Contains(m.Field, "video") {
			t.Fatalf("completeness asks for a video: %+v", m)
		}
	}
}

// Every search adapter returns has_video (and the video) on its results:
// the memory adapter answers from the records, the Elasticsearch adapter's
// hits are re-read from the store, and the index document carries has_video.
func TestSearchResultsCarryHasVideo(t *testing.T) {
	store := NewMemoryStore(noLabels{})
	withVideo := sampleRecord()
	start := 30
	withVideo.YouTubeVideoID, withVideo.YouTubeStartSeconds = "aqz-KE-bpKQ", &start
	store.Put(withVideo)
	without := sampleRecord()
	without.ID, without.Slug, without.Reference = uuid.New(), "no-video", "RP-ZW-10002"
	store.Put(without)

	check := func(adapter string, page Page) {
		t.Helper()
		if len(page.Items) != 2 {
			t.Fatalf("%s: %d items", adapter, len(page.Items))
		}
		for _, l := range page.Items {
			switch l.ID {
			case withVideo.ID:
				if !l.HasVideo || l.Video == nil || l.Video.ID != "aqz-KE-bpKQ" || l.Video.StartSeconds == nil || *l.Video.StartSeconds != 30 ||
					l.Video.EmbedURL != "https://www.youtube-nocookie.com/embed/aqz-KE-bpKQ?rel=0&modestbranding=1&start=30" {
					t.Fatalf("%s: listing with a video: %v %+v", adapter, l.HasVideo, l.Video)
				}
			default:
				if l.HasVideo || l.Video != nil {
					t.Fatalf("%s: listing without a video: %v %+v", adapter, l.HasVideo, l.Video)
				}
			}
		}
		raw, _ := json.Marshal(page.Items[0])
		if !strings.Contains(string(raw), `"has_video":`) || !strings.Contains(string(raw), `"video":`) {
			t.Fatalf("%s: JSON lacks has_video or video: %s", adapter, raw)
		}
	}

	memory := NewService(store, store, noLabels{})
	memory.SetClock(func() time.Time { return esNow })
	page, err := memory.Find(context.Background(), Query{Sort: SortNewest, Page: 1, PageSize: 12})
	if err != nil {
		t.Fatal(err)
	}
	check("memory", page)

	es := &fakeES{respond: func(string, string, []byte) (int, string) {
		return 200, `{"hits":{"total":{"value":2},"hits":[{"_id":"` + withVideo.ID.String() + `"},{"_id":"` + without.ID.String() + `"}]},"aggregations":{}}`
	}}
	elastic := NewService(store, NewElasticSearch(es, "rp365-test", time.Second), noLabels{})
	elastic.SetClock(func() time.Time { return esNow })
	if page, err = elastic.Find(context.Background(), Query{Sort: SortNewest, Page: 1, PageSize: 12}); err != nil {
		t.Fatal(err)
	}
	check("elasticsearch", page)

	if !BuildDocument(withVideo, testLabels).HasVideo || BuildDocument(without, testLabels).HasVideo {
		t.Fatal("the index document must say has_video")
	}
	raw, _ := json.Marshal(BuildDocument(withVideo, testLabels))
	if !strings.Contains(string(raw), `"has_video":true`) || strings.Contains(string(raw), "aqz-KE-bpKQ") {
		t.Fatalf("the document carries has_video, never the video: %s", raw)
	}
}
