package devseed

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

func TestLeadFunnelSamples(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	store := propertyrequests.NewMemoryStore()
	SeedMemoryListings(store)
	// Twice: re-runnable.
	for range 2 {
		if err := SeedMemoryLeadFunnel(ctx, store, now); err != nil {
			t.Fatal(err)
		}
	}
	samples := LeadFunnelRequests(now)
	if len(samples) != LeadFunnelRequestCount || len(store.Requests()) != LeadFunnelRequestCount {
		t.Fatalf("%d samples, %d stored", len(samples), len(store.Requests()))
	}
	for _, s := range samples {
		// Every move is allowed by the inbox's rules and is in the past.
		stage := propertyrequests.StageNewEnquiry
		for _, step := range s.Steps {
			if step.At.After(now) || step.At.Before(s.Request.CreatedAt) {
				t.Fatalf("%s: step at %s", s.Request.Reference, step.At)
			}
			if step.AssignTo != nil {
				continue
			}
			if !slices.Contains(propertyrequests.NextStages(stage), step.Stage) {
				t.Fatalf("%s: %s -> %s is not allowed", s.Request.Reference, stage, step.Stage)
			}
			stage = step.Stage
		}
	}

	from := now.Truncate(24*time.Hour).AddDate(0, 0, -29)
	org := SampleEstates.ID
	report := func(scope propertyrequests.InboxScope, org *uuid.UUID) propertyrequests.Funnel {
		facts, err := store.FunnelFacts(ctx, propertyrequests.FunnelFilter{Scope: scope, OrganisationID: org, From: from, To: from.AddDate(0, 0, 30)})
		if err != nil {
			t.Fatal(err)
		}
		return propertyrequests.ComputeFunnel(facts)
	}
	estates := report(propertyrequests.InboxScope{UserID: EstatesOwnerID, ManagedOrganisationIDs: []uuid.UUID{org}}, &org)
	side, rt := estates.SideStates, estates.ResponseTime
	if estates.Received != 21 || estates.Steps[5].Reached != 4 || side.ConvertedThenClosed != 1 || side.ClosedWithoutConverting != 6 ||
		side.FollowUpRequired.Current != 2 || side.FollowUpRequired.Ever != 3 || rt.NeverResponded != 7 || rt.Responded != 14 || len(estates.ByAgent) != 3 {
		t.Fatalf("Sample Estates: received %d converted %d side %+v response %+v agents %d",
			estates.Received, estates.Steps[5].Reached, side, rt, len(estates.ByAgent))
	}
	grace := report(propertyrequests.InboxScope{UserID: SellerID}, nil)
	if grace.Received != 3 || grace.Steps[5].Reached != 1 {
		t.Fatalf("Grace: received %d converted %d", grace.Received, grace.Steps[5].Reached)
	}
}
