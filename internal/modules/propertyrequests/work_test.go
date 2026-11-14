package propertyrequests

import (
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestStageRules(t *testing.T) {
	if len(Stages()) != 8 {
		t.Fatalf("stages = %v", Stages())
	}
	for _, stage := range Stages() {
		if StageLabel(stage) == stage || !slices.Contains(CustomerStatuses(), CustomerStatus(stage)) {
			t.Fatalf("stage %s has no label or customer status", stage)
		}
		next := NextStages(stage)
		if len(next) == 0 || slices.Contains(next, stage) || slices.Contains(next, StageNewEnquiry) {
			t.Fatalf("moves from %s = %v", stage, next)
		}
	}
	if got := NextStages(StageConverted); len(got) != 1 || got[0] != StageClosed {
		t.Fatalf("moves from CONVERTED = %v", got)
	}
	if slices.Contains(NextStages(StageNewEnquiry), StageConverted) {
		t.Fatal("a new enquiry can be converted without contact")
	}
	// Every customer status is reachable from some stage, and the filter maps back.
	for _, status := range CustomerStatuses() {
		stages := stagesOfCustomerStatuses([]string{status})
		if len(stages) == 0 || CustomerStatusLabel(status) == status {
			t.Fatalf("customer status %s: stages %v", status, stages)
		}
	}
	if stagesOfCustomerStatuses(nil) != nil {
		t.Fatal("no status filter must mean no stage filter")
	}
}

func TestInboxScopeSees(t *testing.T) {
	me, colleague, org, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	request := func(recipient *Recipient) StoredRequest { return StoredRequest{Recipient: recipient} }
	inbox := request(&Recipient{Type: RecipientOrganisation, OrganisationID: &org})
	mine := request(&Recipient{Type: RecipientAgent, UserID: &me, OrganisationID: &org})
	theirs := request(&Recipient{Type: RecipientAgent, UserID: &colleague, OrganisationID: &org})
	sold := request(&Recipient{Type: RecipientSeller, UserID: &me})
	soldByOther := request(&Recipient{Type: RecipientSeller, UserID: &colleague})
	exception := request(nil)

	tests := []struct {
		name  string
		scope InboxScope
		sees  []StoredRequest
		blind []StoredRequest
	}{
		{"manager", InboxScope{UserID: me, ManagedOrganisationIDs: []uuid.UUID{org}}, []StoredRequest{inbox, mine, theirs, sold}, []StoredRequest{soldByOther, exception}},
		{"agent", InboxScope{UserID: me, MemberOrganisationIDs: []uuid.UUID{org}}, []StoredRequest{inbox, mine, sold}, []StoredRequest{theirs, soldByOther, exception}},
		{"other organisation", InboxScope{UserID: me, ManagedOrganisationIDs: []uuid.UUID{other}}, []StoredRequest{sold}, []StoredRequest{inbox, mine, theirs, exception}},
		{"nobody", InboxScope{UserID: other}, nil, []StoredRequest{inbox, mine, theirs, sold, soldByOther, exception}},
	}
	for _, tt := range tests {
		for i, r := range tt.sees {
			if !tt.scope.Sees(r) {
				t.Fatalf("%s does not see request %d", tt.name, i)
			}
		}
		for i, r := range tt.blind {
			if tt.scope.Sees(r) {
				t.Fatalf("%s sees request %d", tt.name, i)
			}
		}
	}
}
