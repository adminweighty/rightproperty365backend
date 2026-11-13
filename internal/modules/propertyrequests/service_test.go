package propertyrequests

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
)

func TestResolveRecipient(t *testing.T) {
	agent, org, seller := uuid.New(), uuid.New(), uuid.New()
	tests := []struct {
		name    string
		listing Listing
		want    string // "" means routing exception
	}{
		{"agent first", Listing{AgentUserID: &agent, AgentEligible: true, OrganisationID: &org, OrganisationEligible: true, SellerUserID: &seller, SellerEligible: true}, RecipientAgent},
		{"ineligible agent falls back to organisation", Listing{AgentUserID: &agent, OrganisationID: &org, OrganisationEligible: true}, RecipientOrganisation},
		{"organisation inbox", Listing{OrganisationID: &org, OrganisationEligible: true}, RecipientOrganisation},
		{"ineligible organisation falls back to seller", Listing{OrganisationID: &org, SellerUserID: &seller, SellerEligible: true}, RecipientSeller},
		{"direct seller", Listing{SellerUserID: &seller, SellerEligible: true}, RecipientSeller},
		{"nobody eligible", Listing{AgentUserID: &agent, OrganisationID: &org, SellerUserID: &seller}, ""},
		{"no owners at all", Listing{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveRecipient(tt.listing)
			switch {
			case tt.want == "" && got != nil:
				t.Fatalf("got %+v, want nil", got)
			case tt.want != "" && (got == nil || got.Type != tt.want):
				t.Fatalf("got %+v, want type %s", got, tt.want)
			}
		})
	}
}

func TestNewReference(t *testing.T) {
	re := regexp.MustCompile(`^RP-[2-9A-HJKMNP-TV-Z]{8}$`)
	for range 200 {
		ref, err := NewReference()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(ref) {
			t.Fatalf("reference %q does not match %s", ref, re)
		}
	}
}

func TestCreate_RetriesOnReferenceCollision(t *testing.T) {
	store := NewMemoryStore()
	refs := []string{"RP-AAAAAAAA", "RP-AAAAAAAA", "RP-BBBBBBBB"}
	i := 0
	svc := NewService(store, WithReferenceGenerator(func() (string, error) {
		ref := refs[i]
		i++
		return ref, nil
	}))

	in := Input{
		ListingID: SeedListingID(2).String(), RequestType: TypeEnquiry, Message: "First",
		PreferredContactMethod: ContactEmail, IdempotencyKey: "collision-key-1",
		GuestContact: &GuestContact{Name: "A", Email: "a@example.com"},
		Consent:      &Consent{PrivacyVersion: "v1", Accepted: true},
	}
	first, err := svc.Create(context.Background(), in, "")
	if err != nil {
		t.Fatal(err)
	}
	in.IdempotencyKey, in.Message = "collision-key-2", "Second"
	second, err := svc.Create(context.Background(), in, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Reference != "RP-AAAAAAAA" || second.Reference != "RP-BBBBBBBB" {
		t.Fatalf("references = %s, %s", first.Reference, second.Reference)
	}
}

func TestCreate_ValidationErrorType(t *testing.T) {
	svc := NewService(NewMemoryStore())
	_, err := svc.Create(context.Background(), Input{}, "")
	var verr *ValidationError
	if !errors.As(err, &verr) || len(verr.Fields) == 0 {
		t.Fatalf("err = %v, want *ValidationError with fields", err)
	}
}
