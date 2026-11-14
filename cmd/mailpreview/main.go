// Command mailpreview renders every email template with sample data so the
// result can be opened in a browser.
//
//	go run ./cmd/mailpreview [-out DIR] [-public-url URL]
//
// It writes <name>.html and <name>.txt for each template plus an index.html
// linking them all, into tmp/mail-preview/ by default (git-ignored).
//
// It sends nothing, reads no .env and needs no API key.
package main

import (
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"

	"rightproperty365.com/backend/internal/platform/mail"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mailpreview:", err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", filepath.Join("tmp", "mail-preview"), "output directory")
	// The web app serves the logo images, so the preview points at a running
	// local web app by default; pass the production URL to see the hosted ones.
	publicURL := flag.String("public-url", "http://localhost:3000", "PUBLIC_WEB_URL used for links and images")
	flag.Parse()

	renderer, err := mail.NewRenderer(*publicURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}

	var index strings.Builder
	index.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>RightProperty365 email previews</title>
<style>body{font-family:Inter,system-ui,-apple-system,"Segoe UI",sans-serif;margin:32px auto;max-width:860px;padding:0 16px;color:#131722}
h1{font-size:24px}h2{font-size:16px;margin-top:32px}table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:8px 10px;border-bottom:1px solid #e6e9f0;font-size:14px;vertical-align:top}
a{color:#1900ac}.tag{display:inline-block;padding:2px 8px;border-radius:999px;font-size:12px;background:#e9fcf8;color:#0a6f5d}.tag.off{background:#fbf1df;color:#8a5208}
p.note{color:#4f586a;font-size:14px}</style></head><body>
<h1>RightProperty365 email previews</h1>
<p class="note">Rendered with sample data. Nothing here was sent. Links and the logo use ` + html.EscapeString(renderer.PublicURL()) + `; the logo only shows when that web app is running.</p>
`)
	section := func(title string, triggered bool) error {
		fmt.Fprintf(&index, "<h2>%s</h2>\n<table><tr><th>Email</th><th>Subject</th><th>Files</th></tr>\n", html.EscapeString(title))
		for _, t := range mail.Templates() {
			if t.Triggered != triggered {
				continue
			}
			msg, err := renderer.Render(t.Name, "preview@example.test", mail.SampleData(t.Name))
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(*out, t.Name+".html"), []byte(msg.HTML), 0o644); err != nil {
				return err
			}
			text := "Subject: " + msg.Subject + "\n\n" + msg.Text
			if err := os.WriteFile(filepath.Join(*out, t.Name+".txt"), []byte(text), 0o644); err != nil {
				return err
			}
			tag := `<span class="tag">sent today</span>`
			if !t.Triggered {
				tag = `<span class="tag off">not yet triggered</span>`
			}
			fmt.Fprintf(&index, "<tr><td>%s<br>%s</td><td>%s</td><td><a href=\"%s.html\">HTML</a> &middot; <a href=\"%s.txt\">text</a></td></tr>\n",
				html.EscapeString(t.Title), tag, html.EscapeString(msg.Subject), t.Name, t.Name)
		}
		index.WriteString("</table>\n")
		return nil
	}
	if err := section("Sent today", true); err != nil {
		return err
	}
	if err := section("Designed and tested, but not yet triggered (the feature is not built)", false); err != nil {
		return err
	}
	index.WriteString("</body></html>\n")
	if err := os.WriteFile(filepath.Join(*out, "index.html"), []byte(index.String()), 0o644); err != nil {
		return err
	}

	abs, err := filepath.Abs(*out)
	if err != nil {
		abs = *out
	}
	fmt.Printf("wrote %d templates (HTML and text) and index.html to %s\n", len(mail.Templates()), abs)
	return nil
}
