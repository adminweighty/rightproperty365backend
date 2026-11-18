package listings

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// YouTube property tours (BR-013, FR-011). An advertiser links a YouTube
// video to a listing by pasting a watch or share link. Only the video's id
// (and where it starts) is stored: never the link as typed, never anything
// fetched from YouTube. The server never calls YouTube (no oEmbed, no
// thumbnail check); whether a video is available is known only in the
// viewer's browser, which shows a fallback when it is not.

// ProviderYouTube is the provider of a listing's video.
const ProviderYouTube = "YOUTUBE"

// MaxYouTubeStartSeconds bounds the start time of a video (24 hours; a
// YouTube video or live archive is shorter).
const MaxYouTubeStartSeconds = 24 * 60 * 60

// maxYouTubeURLLength bounds the link a parser looks at.
const maxYouTubeURLLength = 2048

// MsgNotYouTubeVideo is the field error of a youtube_url that is not a link
// to a YouTube video.
const MsgNotYouTubeVideo = "must be a link to a YouTube video"

// ErrNotYouTubeVideo: the text is not a link to a YouTube video on an
// allowlisted host.
var ErrNotYouTubeVideo = errors.New(MsgNotYouTubeVideo)

// youTubeIDRe is the form of a YouTube video id.
var youTubeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// youTubeTimeRe is a start time: whole seconds ("90", "90s") or hours,
// minutes and seconds ("1h2m3s", "2m", "1m30s").
var youTubeTimeRe = regexp.MustCompile(`^(?:(\d{1,3})h)?(?:(\d{1,4})m)?(?:(\d{1,6})s?)?$`)

// youTubeHosts are the only hosts a video link may name (exact match, lower
// case; no port, no user information).
var youTubeHosts = map[string]bool{
	"youtube.com": true, "www.youtube.com": true, "m.youtube.com": true, "music.youtube.com": true,
	"youtu.be":             true,
	"youtube-nocookie.com": true, "www.youtube-nocookie.com": true,
}

// ValidYouTubeID reports whether id has the form of a YouTube video id.
func ValidYouTubeID(id string) bool { return youTubeIDRe.MatchString(id) }

// ParseYouTubeURL reads the video id, and the start time in seconds (nil
// when the link has none, or starts at 0), from a YouTube watch or share
// link. It accepts http and https links to the allowlisted hosts only, with
// the paths /watch?v=ID, /shorts/ID, /embed/ID, /live/ID and /v/ID (and
// youtu.be/ID), and a start time as t= or start= in seconds or as 1h2m3s.
// Anything else is ErrNotYouTubeVideo: other hosts (look-alikes included),
// playlists, channels, handles, markup, other schemes, user information,
// percent-encoded hosts or paths, ports, and ids that are not 11 characters.
// It is a pure function: nothing is fetched.
func ParseYouTubeURL(raw string) (id string, start *int, err error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > maxYouTubeURLLength {
		return "", nil, ErrNotYouTubeVideo
	}
	for _, c := range s {
		// No spaces, controls, markup or quotes anywhere in a link.
		if c <= ' ' || c == 0x7f || strings.ContainsRune(`<>"'\{}|^`+"`", c) {
			return "", nil, ErrNotYouTubeVideo
		}
	}
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok || (!strings.EqualFold(scheme, "https") && !strings.EqualFold(scheme, "http")) {
		return "", nil, ErrNotYouTubeVideo
	}
	// The authority as typed: no user information, no percent-encoding,
	// no port.
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if authority == "" || strings.ContainsAny(authority, "@%:[]") {
		return "", nil, ErrNotYouTubeVideo
	}
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Opaque != "" || u.Port() != "" {
		return "", nil, ErrNotYouTubeVideo
	}
	host := strings.ToLower(u.Host)
	if host != strings.ToLower(authority) || !youTubeHosts[host] {
		return "", nil, ErrNotYouTubeVideo
	}
	// The path as typed: no percent-encoding, no empty or dot segments.
	path := u.EscapedPath()
	if strings.Contains(path, "%") || strings.Contains(path, "//") || strings.Contains(path, "/.") {
		return "", nil, ErrNotYouTubeVideo
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", nil, ErrNotYouTubeVideo
	}
	segments := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/"), "/")
	switch {
	case host == "youtu.be":
		if len(segments) == 1 {
			id = segments[0]
		}
	case len(segments) == 1 && segments[0] == "watch" && !strings.HasSuffix(path, "/"):
		if v := query["v"]; len(v) == 1 {
			id = v[0]
		}
	case len(segments) == 2 && (segments[0] == "shorts" || segments[0] == "embed" || segments[0] == "live" || segments[0] == "v"):
		id = segments[1]
	}
	if !ValidYouTubeID(id) {
		return "", nil, ErrNotYouTubeVideo
	}
	for _, key := range []string{"t", "start"} {
		values := query[key]
		if len(values) == 0 {
			continue
		}
		if len(values) > 1 {
			return "", nil, ErrNotYouTubeVideo
		}
		seconds, ok := parseYouTubeTime(values[0])
		if !ok {
			return "", nil, ErrNotYouTubeVideo
		}
		if seconds > 0 {
			start = &seconds
		}
		break
	}
	return id, start, nil
}

// parseYouTubeTime reads "90", "90s" or "1h2m3s" as seconds.
func parseYouTubeTime(v string) (int, bool) {
	m := youTubeTimeRe.FindStringSubmatch(v)
	if v == "" || m == nil {
		return 0, false
	}
	total := 0
	for i, unit := range []int{3600, 60, 1} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return 0, false
		}
		total += n * unit
	}
	if total > MaxYouTubeStartSeconds {
		return 0, false
	}
	return total, true
}

// YouTubeWatchURL is the canonical watch link of a video:
// https://www.youtube.com/watch?v=ID.
func YouTubeWatchURL(id string) string { return "https://www.youtube.com/watch?v=" + id }

// youTubeEditURL is what the editor shows as youtube_url: the canonical
// watch link, with &t=Ns when the video starts later, so that sending it
// back keeps the start time.
func youTubeEditURL(id string, start *int) string {
	out := YouTubeWatchURL(id)
	if start != nil && *start > 0 {
		out += "&t=" + strconv.Itoa(*start) + "s"
	}
	return out
}

// YouTubeEmbedURL is the privacy-enhanced embed address of a video, without
// related videos from other channels, starting at start when set.
func YouTubeEmbedURL(id string, start *int) string {
	out := "https://www.youtube-nocookie.com/embed/" + id + "?rel=0&modestbranding=1"
	if start != nil && *start > 0 {
		out += "&start=" + strconv.Itoa(*start)
	}
	return out
}

// YouTubeThumbnailURL is the high-quality still YouTube serves for a video.
func YouTubeThumbnailURL(id string) string { return "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg" }

// Video is the public video tour of a listing.
type Video struct {
	// Provider is YOUTUBE.
	Provider     string `json:"provider"`
	ID           string `json:"id"`
	StartSeconds *int   `json:"start_seconds"`
	// EmbedURL is the iframe address (youtube-nocookie, rel=0); WatchURL the
	// link to the video on YouTube; ThumbnailURL the poster image.
	EmbedURL     string `json:"embed_url"`
	WatchURL     string `json:"watch_url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

// videoOf is the public video of a record, nil when it has none.
func videoOf(id string, start *int) *Video {
	if !ValidYouTubeID(id) {
		return nil
	}
	return &Video{Provider: ProviderYouTube, ID: id, StartSeconds: copyInt(start), EmbedURL: YouTubeEmbedURL(id, start),
		WatchURL: YouTubeWatchURL(id), ThumbnailURL: YouTubeThumbnailURL(id)}
}
