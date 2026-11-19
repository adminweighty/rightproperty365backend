package httpx

import "net/http"

// Change sources taken from the X-Client request header. They are recorded
// with a person's own preference changes: notification preferences (API
// 0.35.0) and marketing consent (API 0.37.1).
const (
	ClientSourceWeb = "web"
	ClientSourceApp = "app"
	ClientSourceAPI = "api"
)

// ClientHeader is the request header a client names itself in.
const ClientHeader = "X-Client"

// SourceFromClient maps an X-Client header value to a change source:
// "web" is web; "app", "ios" and "android" are app; anything else, or no
// header, is api.
func SourceFromClient(h string) string {
	switch h {
	case "web":
		return ClientSourceWeb
	case "app", "ios", "android":
		return ClientSourceApp
	}
	return ClientSourceAPI
}

// ClientSource is SourceFromClient of the request's X-Client header.
func ClientSource(r *http.Request) string { return SourceFromClient(r.Header.Get(ClientHeader)) }
