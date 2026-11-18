package devseed

import (
	"testing"
	"time"

	"rightproperty365.com/backend/internal/modules/applications"
)

// The sample questionnaire is valid, has two versions (the submitted
// application on the old one), and is attached to two rent listings.
func TestApplicationSamples(t *testing.T) {
	s := ApplicationSamplesAt(time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC))
	if len(s.Forms) != 2 || len(s.Intakes) != 2 || len(s.Applications) != 2 {
		t.Fatalf("%d forms, %d intakes, %d applications", len(s.Forms), len(s.Intakes), len(s.Applications))
	}
	for _, f := range s.Forms {
		if err := applications.ValidateSections(f.Sections, true); err != nil {
			t.Fatalf("version %d: %v", f.Version, err)
		}
		if w := applications.Warnings(f.Sections); len(w) != 0 {
			t.Fatalf("version %d warns: %+v", f.Version, w)
		}
	}
	if s.Forms[0].Status != applications.FormRetired || s.Forms[1].Status != applications.FormPublished {
		t.Fatal("v1 retired, v2 published")
	}
	sub, draft := s.Applications[0], s.Applications[1]
	if sub.Status != applications.StatusSubmitted || sub.FormID != RentalFormV1ID || sub.ApplicantUserID == nil || *sub.ApplicantUserID != TenantID {
		t.Fatalf("submitted %+v", sub)
	}
	if missing := applications.MissingRequired(s.Forms[0], sub.Answers); len(missing) != 0 {
		t.Fatalf("the submitted sample misses %v", missing)
	}
	if draft.Status != applications.StatusDraft || draft.FormID != RentalFormV2ID || draft.ListingID == sub.ListingID {
		t.Fatalf("draft %+v", draft)
	}
	for _, in := range s.Intakes {
		if in.FormID != RentalFormV2ID || in.Status != applications.IntakeOpen {
			t.Fatalf("intake %+v", in)
		}
	}
}
