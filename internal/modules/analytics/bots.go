package analytics

import (
	"net/netip"
	"strings"
	"sync"
	"time"
)

// botMarkers are lower-case fragments of user agents that are not people:
// search and social crawlers, headless and automated browsers, HTTP
// libraries and monitoring tools. The list is maintained here; a match marks
// the event bot=true (stored, excluded from every metric, counted as
// filtered).
var botMarkers = []string{
	// Generic words used by most crawlers.
	"bot", "crawler", "spider", "crawl", "slurp", "scraper", "archiver", "indexer",
	// Search engines and SEO tools that do not say "bot".
	"mediapartners-google", "adsbot", "feedfetcher", "google-inspectiontool", "googleother", "bingpreview",
	"yandex", "baiduspider", "sogou", "exabot", "ia_archiver", "semrush", "ahrefs", "mj12", "dotbot", "petalbot",
	"bytespider", "gptbot", "ccbot", "claudebot", "perplexity", "applebot",
	// Social link previews.
	"facebookexternalhit", "facebot", "twitterbot", "linkedinbot", "whatsapp", "telegrambot", "slackbot",
	"discordbot", "embedly", "skypeuripreview",
	// Headless and automated browsers.
	"headless", "phantomjs", "puppeteer", "playwright", "selenium", "webdriver", "lighthouse",
	"chrome-lighthouse", "pagespeed", "gtmetrix", "prerender",
	// HTTP clients and libraries.
	"curl/", "wget/", "python-requests", "python-urllib", "aiohttp", "httpx", "go-http-client", "java/",
	"okhttp", "apache-httpclient", "libwww-perl", "node-fetch", "axios/", "undici", "postmanruntime",
	"insomnia", "httpie", "scrapy",
	// Uptime and monitoring.
	"pingdom", "uptimerobot", "statuscake", "site24x7", "newrelicpinger", "datadog", "monitor", "nagios",
	"zabbix", "check_http", "freshping", "betteruptime", "uptime-kuma",
}

// IsBotUserAgent reports whether a user agent is a known bot. An empty user
// agent is a bot too: every real browser sends one.
func IsBotUserAgent(ua string) bool {
	ua = strings.ToLower(strings.TrimSpace(ua))
	if ua == "" {
		return true
	}
	for _, m := range botMarkers {
		if strings.Contains(ua, m) {
			return true
		}
	}
	return false
}

// TruncateIP returns the network of an address as stored: IPv4 /24
// ("203.0.113.0/24"), IPv6 /48 ("2001:db8:abcd::/48"). An IPv4-mapped IPv6
// address is treated as IPv4. "" for something that is not an address.
// The full address is never kept.
func TruncateIP(raw string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	addr = addr.Unmap().WithZone("")
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return ""
	}
	return prefix.String()
}

// rateDetector counts events per key (a session, a network) in one-minute
// buckets over RateWindow. It is held in process memory, like
// httpx.RateLimiter: with several API instances each counts its own share.
type rateDetector struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	counts    map[string]map[int64]int
	lastSweep time.Time
}

func newRateDetector(limit int, window time.Duration) *rateDetector {
	return &rateDetector{limit: limit, window: window, counts: map[string]map[int64]int{}}
}

// over counts one event for every key and reports whether any of them is now
// over the limit within the window.
func (d *rateDetector) over(now time.Time, keys ...string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweep(now)
	minute := now.Unix() / 60
	oldest := minute - int64(d.window/time.Minute) + 1
	exceeded := false
	for _, k := range keys {
		if k == "" {
			continue
		}
		buckets := d.counts[k]
		if buckets == nil {
			buckets = map[int64]int{}
			d.counts[k] = buckets
		}
		buckets[minute]++
		total := 0
		for m, n := range buckets {
			if m < oldest {
				delete(buckets, m)
				continue
			}
			total += n
		}
		if total > d.limit {
			exceeded = true
		}
	}
	return exceeded
}

// sweep drops keys with no recent bucket, so the map cannot grow without bound.
func (d *rateDetector) sweep(now time.Time) {
	if now.Sub(d.lastSweep) < time.Minute {
		return
	}
	d.lastSweep = now
	oldest := now.Unix()/60 - int64(d.window/time.Minute) + 1
	for k, buckets := range d.counts {
		recent := false
		for m := range buckets {
			if m >= oldest {
				recent = true
				break
			}
		}
		if !recent {
			delete(d.counts, k)
		}
	}
}
