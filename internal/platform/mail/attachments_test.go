package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

var testInvite = Attachment{
	Filename: "appointment-RPA-2026-000123.ics", ContentType: "text/calendar; charset=utf-8; method=REQUEST",
	Content: []byte("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nSUMMARY:secret-body-marker\r\nEND:VCALENDAR\r\n"),
}

// Resend receives attachments as [{filename, content (base64), content_type}].
func TestResendSendsAttachments(t *testing.T) {
	f := newFakeResend(t, okResponse)
	s, _ := f.sender(t, nil)
	m := testMessage
	m.Attachments = []Attachment{testInvite}
	if _, err := s.Send(context.Background(), testKey, m); err != nil {
		t.Fatal(err)
	}
	list, ok := f.requests[0].Body["attachments"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("attachments = %#v", f.requests[0].Body["attachments"])
	}
	a := list[0].(map[string]any)
	if len(a) != 3 || a["filename"] != testInvite.Filename || a["content_type"] != testInvite.ContentType {
		t.Fatalf("attachment = %#v", a)
	}
	raw, err := base64.StdEncoding.DecodeString(a["content"].(string))
	if err != nil || !bytes.Equal(raw, testInvite.Content) {
		t.Fatalf("content does not round-trip: %v", err)
	}

	// Without attachments the field is absent, as before.
	f2 := newFakeResend(t, okResponse)
	s2, _ := f2.sender(t, nil)
	if _, err := s2.Send(context.Background(), testKey, testMessage); err != nil {
		t.Fatal(err)
	}
	if _, ok := f2.requests[0].Body["attachments"]; ok {
		t.Fatal("an email without attachments must not send the field")
	}
}

// The development transport logs an attachment's name, type and size, never
// its content.
func TestLogSenderLogsAttachmentMetadataOnly(t *testing.T) {
	var out bytes.Buffer
	s := NewLogSender(slog.New(slog.NewJSONHandler(&out, nil)))
	m := testMessage
	m.Attachments = []Attachment{testInvite}
	if _, err := s.Send(context.Background(), testKey, m); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "secret-body-marker") || strings.Contains(out.String(), base64.StdEncoding.EncodeToString(testInvite.Content)[:20]) {
		t.Fatalf("the log carries the attachment's content: %s", out.String())
	}
	var line struct {
		Attachments []AttachmentSummary `json:"attachments"`
	}
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	want := AttachmentSummary{Filename: testInvite.Filename, ContentType: testInvite.ContentType, Size: len(testInvite.Content)}
	if len(line.Attachments) != 1 || line.Attachments[0] != want {
		t.Fatalf("attachments logged = %+v, want %+v", line.Attachments, want)
	}
}

// The renderer copies Data.Attachments onto the message.
func TestRenderCarriesAttachments(t *testing.T) {
	r, err := NewRenderer("https://rightproperty365.com")
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.Render("appointment-confirmed", "a@example.test", Data{Reference: "RPA-1", Attachments: []Attachment{testInvite}})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != testInvite.Filename {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
}
