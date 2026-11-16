package agreements

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckStamp(t *testing.T) {
	clean, err := CheckStamp(Stamp{TopText: "  Acme Properties (Pvt) Ltd ", CentreLines: []string{"Harare", " ", "Zimbabwe"}, BottomText: "reg no 1234/2020"})
	if err != nil {
		t.Fatal(err)
	}
	if clean.Shape != StampRound || clean.Colour != InkBlue || clean.TopText != "ACME PROPERTIES (PVT) LTD" ||
		strings.Join(clean.CentreLines, "|") != "HARARE|ZIMBABWE" || clean.BottomText != "REG NO 1234/2020" {
		t.Fatalf("cleaned %+v", clean)
	}
	bad := Stamp{Shape: "HEXAGON", Colour: "PINK", TopText: "", CentreLines: []string{"A", "B", "C", "D"},
		BottomText: strings.Repeat("x", MaxStampTextRunes+1)}
	_, err = CheckStamp(bad)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("want a validation error, got %v", err)
	}
	for _, key := range []string{"stamp.shape", "stamp.colour", "stamp.top_text", "stamp.centre_lines", "stamp.bottom_text"} {
		if verr.Fields[key] == "" {
			t.Errorf("no message for %s: %v", key, verr.Fields)
		}
	}
	_, err = CheckStamp(Stamp{TopText: strings.Repeat("A", MaxStampTextRunes+1), CentreLines: []string{strings.Repeat("B", MaxStampLineRunes+1)}})
	if !errors.As(err, &verr) || verr.Fields["stamp.top_text"] == "" || verr.Fields["stamp.centre_lines[0]"] == "" {
		t.Fatalf("too long: %v", err)
	}
	if _, err := CheckStamp(Stamp{TopText: "<b>ACME</b>"}); err == nil {
		t.Fatal("markup must be refused")
	}
	if got := StampDate(time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)); got != "16 NOV 2026" {
		t.Fatalf("date %q", got)
	}
}

func TestStampFontFallsBack(t *testing.T) {
	if stampFontFor("ACME PROPERTIES (PVT) LTD") != stampPrimary || stampFontFor("ЗОЯ ГОМЕЗ") != stampPrimary {
		t.Fatal("Latin and Cyrillic are set in the condensed bold")
	}
	if stampFontFor("王小明地产") != stampFallback {
		t.Fatal("a text the condensed bold cannot set falls back to Go Bold")
	}
}

// Every shape renders a transparent PNG of about 600 pixels with ink on it,
// including a text the main font cannot set; the same input gives the same bytes.
func TestRenderStamp(t *testing.T) {
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	dump := os.Getenv("STAMP_DUMP_DIR")
	for name, s := range map[string]Stamp{
		"round":     {Shape: StampRound, Colour: InkBlue, TopText: "ACME PROPERTIES (PVT) LTD", CentreLines: []string{"HARARE"}, BottomText: "REG NO 1234/2020", IncludeDate: true},
		"round-top": {Shape: StampRound, Colour: InkRed, TopText: "ZOË MÜLLER-ŚMIAŁEK ESTATES", CentreLines: []string{"LANDLORD", "BULAWAYO"}},
		"oval":      {Shape: StampOval, Colour: InkPurple, TopText: "SUNRISE HOLIDAY HOMES LIMITED", CentreLines: []string{"LONDON"}, BottomText: "COMPANY NO 01234567", IncludeDate: true},
		"rectangle": {Shape: StampRectangle, Colour: InkGreen, TopText: "KAREN RENTALS", CentreLines: []string{"P.O. BOX 1234", "NAIROBI"}, BottomText: "PIN A001234567Z", IncludeDate: true},
		"cjk":       {Shape: StampRound, Colour: InkBlack, TopText: "王小明地产有限公司", CentreLines: []string{"上海"}, IncludeDate: true},
	} {
		clean, err := CheckStamp(s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		content, err := RenderStamp(clean, day)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		img, err := png.Decode(bytes.NewReader(content))
		if err != nil {
			t.Fatalf("%s: not a PNG: %v", name, err)
		}
		b := img.Bounds()
		if max(b.Dx(), b.Dy()) != StampPreviewLongSide || min(b.Dx(), b.Dy()) < 200 {
			t.Fatalf("%s: %v", name, b)
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Fatalf("%s: the corner must be transparent", name)
		}
		inked := 0
		for y := b.Min.Y; y < b.Max.Y; y += 2 {
			for x := b.Min.X; x < b.Max.X; x += 2 {
				if _, _, _, a := img.At(x, y).RGBA(); a > 0x8000 {
					inked++
				}
			}
		}
		if inked < 2000 {
			t.Fatalf("%s: only %d inked samples", name, inked)
		}
		again, _ := RenderStamp(clean, day)
		if !bytes.Equal(content, again) {
			t.Fatalf("%s: rendering is not deterministic", name)
		}
		if dump != "" {
			_ = os.WriteFile(filepath.Join(dump, name+".png"), content, 0o644)
		}
	}
}

// A required STAMP field needs a stamp on submit; the stamp is kept with
// the signer, audited, and drawn into the completed document.
func TestSubmitRequiresStamp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	name := "Stamped contract"
	v, err := h.svc.Create(ctx, h.p, Input{Name: &name, Recipients: &[]RecipientInput{
		{Name: "A", Email: "a@example.test", Role: RoleSigner, Order: 1}, {Name: "B", Email: "b@example.test", Role: RoleApprover, Order: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if v, err = h.svc.AddDocument(ctx, h.p, v.ID, "c.pdf", testPDF(t, 1)); err != nil {
		t.Fatal(err)
	}
	signer, approver := v.Recipients[0].ID.String(), v.Recipients[1].ID.String()
	var verr *ValidationError
	// A STAMP is a signer's, and at least 57 points.
	_, err = h.svc.PutFields(ctx, h.p, v.ID, []FieldInput{
		{RecipientID: signer, Type: FieldSignature, Page: 1, X: 72, Y: 600, W: 200, H: 40},
		{RecipientID: signer, Type: FieldStamp, Page: 1, X: 300, Y: 500, W: 40, H: 40},
		{RecipientID: approver, Type: FieldStamp, Page: 1, X: 400, Y: 500, W: StampFieldSize, H: StampFieldSize}})
	if !errors.As(err, &verr) || verr.Fields["fields[1]"] == "" || verr.Fields["fields[2].type"] == "" {
		t.Fatalf("stamp field rules: %v", err)
	}
	if _, err := h.svc.PutFields(ctx, h.p, v.ID, []FieldInput{
		{RecipientID: signer, Type: FieldSignature, Page: 1, X: 72, Y: 600, W: 200, H: 40},
		{RecipientID: signer, Type: FieldStamp, Page: 1, X: 300, Y: 480, W: StampFieldSize, H: StampFieldSize}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Send(ctx, h.p, v.ID); err != nil {
		t.Fatal(err)
	}
	token := h.mail.signing[0].Token
	view, err := h.svc.Signing(ctx, token, "", "")
	if err != nil || len(view.Fields) != 2 || view.Fields[1].Type != FieldStamp || !view.Fields[1].Required {
		t.Fatalf("signing view %+v %v", view.Fields, err)
	}
	if _, err := h.svc.SendCode(ctx, token, "192.0.2.7", "UA"); err != nil {
		t.Fatal(err)
	}
	code := h.mail.codes["a@example.test"]
	sig := &AdoptInput{Kind: AdoptTyped, Text: "Anne Signer"}
	_, err = h.svc.Submit(ctx, token, code, SubmitInput{Consent: true, Signature: sig}, "192.0.2.7", "UA")
	if !errors.As(err, &verr) || verr.Fields["stamp"] == "" {
		t.Fatalf("a required stamp: %v", err)
	}
	_, err = h.svc.Submit(ctx, token, code, SubmitInput{Consent: true, Signature: sig, Stamp: &Stamp{Shape: "ROUND"}}, "192.0.2.7", "UA")
	if !errors.As(err, &verr) || verr.Fields["stamp.top_text"] == "" {
		t.Fatalf("an invalid stamp: %v", err)
	}
	if _, err := h.svc.Submit(ctx, token, code, SubmitInput{Consent: true, Signature: sig,
		Stamp: &Stamp{Shape: StampOval, Colour: InkRed, TopText: "Anne Signer Holdings", IncludeDate: true}}, "192.0.2.7", "UA"); err != nil {
		t.Fatal(err)
	}
	a, err := h.svc.store.Agreement(ctx, v.ID)
	if err != nil || a.Recipients[0].Stamp == nil || a.Recipients[0].Stamp.TopText != "ANNE SIGNER HOLDINGS" {
		t.Fatalf("stamp kept %+v %v", a.Recipients[0].Stamp, err)
	}
	events, _ := h.svc.store.Events(ctx, v.ID)
	lines := auditLines(a, events)
	found := false
	for _, l := range lines {
		found = found || l.Text == "Stamp applied: ANNE SIGNER HOLDINGS"
	}
	if !found {
		t.Fatalf("audit lines %+v", lines)
	}
	// The completed document carries the stamp as a transparent image.
	original := testPDF(t, 1)
	pages, _ := InspectPDF(original)
	content, err := RenderFinal(FinalInput{Original: original, Pages: pages, Fields: a.Fields,
		Recipients: map[string]Recipient{a.Recipients[0].ID.String(): a.Recipients[0]}, Images: map[string][]byte{},
		Audit: AuditReport{Name: a.Name, Reference: a.Reference, CompletedAt: h.now, Pages: 1, Lines: lines}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte("/SMask")) || !bytes.Contains(content, []byte("/Subtype /Image")) {
		t.Fatal("the stamp is drawn as an image with transparency")
	}
}
