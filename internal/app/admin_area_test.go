package app

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/accounts"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/adminaudit"
)

func (m *captureMailer) SendAccountDeleted(_ context.Context, to, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletions = append(m.deletions, to)
	return nil
}

func (m *captureMailer) deletionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.deletions)
}

func (e *env) del(path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	return e.do(http.MethodDelete, path, body, opts...)
}

type adminUserDetail struct {
	User struct {
		ID               string         `json:"id"`
		Name             string         `json:"name"`
		Email            string         `json:"email"`
		Status           string         `json:"status"`
		SuspensionReason *string        `json:"suspension_reason"`
		ListingCounts    map[string]int `json:"listing_counts"`
		Memberships      []struct {
			OrganisationName string `json:"organisation_name"`
			Role             string `json:"role"`
			Status           string `json:"status"`
		} `json:"memberships"`
		AccountRoles []struct {
			Role   string `json:"role"`
			Status string `json:"status"`
		} `json:"account_roles"`
	} `json:"user"`
	Subscriptions []struct {
		InvoiceNumber string `json:"invoice_number"`
		Status        string `json:"status"`
	} `json:"subscriptions"`
	Audit []struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
	} `json:"audit"`
}

func TestAdminUsersListAndDetail(t *testing.T) {
	e := newEnv(t)
	support := e.asAdmin(devseed.SupportEmail)

	rec := e.get("/v1/admin/users?q=tendai&page_size=5", support)
	wantStatus(t, rec, http.StatusOK)
	page := decode[struct {
		Items []struct {
			ID     string `json:"id"`
			Email  string `json:"email"`
			Status string `json:"status"`
		} `json:"items"`
		Total int `json:"total"`
	}](t, rec)
	if page.Total < 1 || page.Items[0].Email != identity.SeedAgentEmail {
		t.Fatalf("search: %+v", page)
	}
	rec = e.get("/v1/admin/users?role=SUPER_ADMIN", support)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), identity.SeedAdminEmail) {
		t.Fatalf("role filter: %s", rec.Body)
	}
	wantError(t, e.get("/v1/admin/users?status=GONE", support), http.StatusBadRequest, "VALIDATION_FAILED")

	rec = e.get("/v1/admin/users/"+identity.SeedAgentUserID.String(), support)
	wantStatus(t, rec, http.StatusOK)
	d := decode[adminUserDetail](t, rec)
	if d.User.Status != "ACTIVE" || len(d.User.Memberships) == 0 || d.User.ListingCounts["PUBLISHED"] == 0 {
		t.Fatalf("detail: %+v", d.User)
	}
	// The seeded request for Agent Starter waits for payment.
	if len(d.Subscriptions) != 1 || d.Subscriptions[0].Status != subscriptions.StatusPendingPayment {
		t.Fatalf("subscriptions: %+v", d.Subscriptions)
	}
	wantError(t, e.get("/v1/admin/users/"+uuid.NewString(), support), http.StatusNotFound, accounts.CodeUserNotFound)

	// Reading is not acting: SUPPORT may not suspend; a MODERATOR or a
	// customer session may not read the list at all.
	wantError(t, e.post("/v1/admin/users/"+identity.SeedAgentUserID.String()+"/suspend", obj{"reason": "Reported listings"}, support),
		http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.get("/v1/admin/users", e.asAdmin(devseed.ModeratorEmail)), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.get("/v1/admin/users", browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))), http.StatusForbidden, "FORBIDDEN")
}

func TestAdminSuspendAndReactivate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin := e.asAdmin(devseed.PlatformAdminEmail)
	seller := devseed.SellerID.String()

	records, err := e.listings.ByIDs(ctx, []uuid.UUID{propertyrequests.SeedListingID(6)})
	if err != nil || len(records) != 1 || records[0].SellerUserID == nil || *records[0].SellerUserID != devseed.SellerID {
		t.Fatalf("the seller's listing: %+v %v", records, err)
	}
	slug := records[0].Slug
	wantStatus(t, e.get("/v1/listings/"+slug), http.StatusOK)

	sellerCookie := e.loginCookie(devseed.SellerEmail, seedPassword)
	wantStatus(t, e.get("/v1/me", browser(sellerCookie)), http.StatusOK)

	wantError(t, e.post("/v1/admin/users/"+seller+"/suspend", obj{"reason": "x"}, admin), http.StatusBadRequest, "VALIDATION_FAILED")
	rec := e.post("/v1/admin/users/"+seller+"/suspend", obj{"reason": "Asked customers for a deposit before any viewing."}, admin)
	wantStatus(t, rec, http.StatusOK)
	d := decode[adminUserDetail](t, rec)
	if d.User.Status != "SUSPENDED" || d.User.SuspensionReason == nil || len(d.Audit) != 1 || d.Audit[0].Action != adminaudit.ActionUserSuspended {
		t.Fatalf("suspended: %+v", d)
	}
	// Every session ends at once and signing in is refused.
	wantError(t, e.get("/v1/me", browser(sellerCookie)), http.StatusUnauthorized, "UNAUTHENTICATED")
	wantError(t, e.post("/v1/auth/login", obj{"email": devseed.SellerEmail, "password": seedPassword}), http.StatusUnauthorized, identity.CodeInvalidCredentials)
	// The listing stays PUBLISHED but leaves the public catalogue.
	wantStatus(t, e.get("/v1/listings/"+slug), http.StatusNotFound)
	if again, _ := e.listings.ByIDs(ctx, []uuid.UUID{propertyrequests.SeedListingID(6)}); again[0].Status != "PUBLISHED" {
		t.Fatalf("status = %s, want PUBLISHED", again[0].Status)
	}
	// Twice is a conflict; the email waits in the outbox.
	wantError(t, e.post("/v1/admin/users/"+seller+"/suspend", obj{"reason": "Again and again"}, admin), http.StatusConflict, accounts.CodeUserState)
	found := false
	for _, ev := range e.identity.Outbox() {
		found = found || (ev.Type == accounts.EventAccountSuspended && ev.AggregateID == devseed.SellerID)
	}
	if !found {
		t.Fatal("no ACCOUNT_SUSPENDED event")
	}

	rec = e.post("/v1/admin/users/"+seller+"/reactivate", nil, admin)
	wantStatus(t, rec, http.StatusOK)
	if d := decode[adminUserDetail](t, rec); d.User.Status != "ACTIVE" || d.User.SuspensionReason != nil {
		t.Fatalf("reactivated: %+v", d.User)
	}
	wantStatus(t, e.get("/v1/listings/"+slug), http.StatusOK)
	wantStatus(t, e.get("/v1/me", browser(e.loginCookie(devseed.SellerEmail, seedPassword))), http.StatusOK)

	// Never one's own account; an administrator account only by a SUPER_ADMIN.
	wantError(t, e.post("/v1/admin/users/"+devseed.PlatformAdminID.String()+"/suspend", obj{"reason": "Testing myself"}, admin),
		http.StatusForbidden, accounts.CodeSelfActionForbidden)
	wantError(t, e.post("/v1/admin/users/"+devseed.SupportID.String()+"/suspend", obj{"reason": "Testing a colleague"}, admin),
		http.StatusForbidden, accounts.CodeAdminAccountProtected)
	wantStatus(t, e.post("/v1/admin/users/"+devseed.SupportID.String()+"/suspend", obj{"reason": "Left the company"},
		e.asAdmin(identity.SeedAdminEmail)), http.StatusOK)
}

func TestAdminDeleteAccount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin := e.asAdmin(devseed.PlatformAdminEmail)

	// The only owner of an agency that has members cannot be deleted.
	rec := e.post("/v1/admin/users/"+devseed.EstatesOwnerID.String()+"/delete", obj{"reason": "Asked by email"}, admin)
	wantError(t, rec, http.StatusConflict, accounts.CodeSoleOwner)
	if !strings.Contains(rec.Body.String(), devseed.SampleEstates.Name) {
		t.Fatalf("the organisation is named: %s", rec.Body)
	}

	agent := identity.SeedAgentUserID
	agentCookie := e.loginCookie(identity.SeedAgentEmail, seedPassword)
	rec = e.post("/v1/admin/users/"+agent.String()+"/delete", obj{"reason": "Asked to be forgotten (support ticket 42)."}, admin)
	wantStatus(t, rec, http.StatusOK)
	out := decode[struct {
		Status                 string `json:"status"`
		OrganisationsLeft      int    `json:"organisations_left"`
		ListingsUnassigned     int    `json:"listings_unassigned"`
		SubscriptionsCancelled int    `json:"subscriptions_cancelled"`
	}](t, rec)
	if out.Status != "DELETED" || out.OrganisationsLeft != 1 || out.ListingsUnassigned < 3 || out.SubscriptionsCancelled != 1 {
		t.Fatalf("deleted: %+v", out)
	}
	wantError(t, e.get("/v1/me", browser(agentCookie)), http.StatusUnauthorized, "UNAUTHENTICATED")
	if e.mail.deletionCount() != 1 || e.mail.deletions[0] != identity.SeedAgentEmail {
		t.Fatalf("deletion notice: %v", e.mail.deletions)
	}
	// Anonymised in place; the organisation keeps its listings, without the agent.
	a, err := e.identity.AdminAccount(agent)
	if err != nil || a.Status != identity.StatusDeleted || a.Name != accounts.DeletedName || a.Email != accounts.DeletedEmail(agent) || a.PasswordHash != "" {
		t.Fatalf("anonymised: %+v %v", a, err)
	}
	if _, err := e.identity.UserByEmail(ctx, identity.SeedAgentEmail); err == nil {
		t.Fatal("the address must be gone")
	}
	records, _ := e.listings.ByIDs(ctx, []uuid.UUID{propertyrequests.SeedListingID(1)})
	if records[0].AgentUserID != nil || records[0].OrganisationID == nil {
		t.Fatalf("listing 1: %+v", records[0])
	}
	wantError(t, e.post("/v1/admin/users/"+agent.String()+"/delete", obj{"reason": "Once more please"}, admin), http.StatusConflict, accounts.CodeUserState)
	var deleted bool
	for _, ev := range e.audit.All() {
		deleted = deleted || (ev.Action == adminaudit.ActionUserDeleted && ev.TargetID == agent.String())
	}
	if !deleted {
		t.Fatal("no audit record")
	}
}

func TestSelfServiceAccountDeletion(t *testing.T) {
	e := newEnv(t)
	cookie := e.loginCookie(devseed.SellerEmail, seedPassword)
	wantError(t, e.del("/v1/me", obj{"password": "not-the-password"}, browser(cookie)), http.StatusUnauthorized, accounts.CodeInvalidPassword)
	wantError(t, e.del("/v1/me", obj{"password": seedPassword}), http.StatusUnauthorized, "UNAUTHENTICATED")

	rec := e.del("/v1/me", obj{"password": seedPassword}, browser(cookie))
	wantStatus(t, rec, http.StatusOK)
	if c := sessionCookie(rec); c == nil || c.MaxAge >= 0 {
		t.Fatal("the session cookie must be cleared")
	}
	wantError(t, e.get("/v1/me", browser(cookie)), http.StatusUnauthorized, "UNAUTHENTICATED")
	wantError(t, e.post("/v1/auth/login", obj{"email": devseed.SellerEmail, "password": seedPassword}), http.StatusUnauthorized, identity.CodeInvalidCredentials)
	records, _ := e.listings.ByIDs(context.Background(), []uuid.UUID{propertyrequests.SeedListingID(6)})
	if records[0].Status != "ARCHIVED" {
		t.Fatalf("the seller's listing is %s, want ARCHIVED", records[0].Status)
	}
}

type subscriptionBody struct {
	Subscription struct {
		ID                  string     `json:"id"`
		InvoiceNumber       string     `json:"invoice_number"`
		Status              string     `json:"status"`
		Amount              string     `json:"amount"`
		CurrencyCode        string     `json:"currency_code"`
		StartsAt            *time.Time `json:"starts_at"`
		EndsAt              *time.Time `json:"ends_at"`
		RenewalOf           *string    `json:"renewal_of"`
		RefundDue           bool       `json:"refund_due"`
		PaymentInstructions *string    `json:"payment_instructions"`
	} `json:"subscription"`
}

func TestSubscriptionLifecycle(t *testing.T) {
	e := newEnv(t)
	// Sessions are reused (the sign-in endpoints are rate limited) and
	// started again whenever the clock moves past their idle timeout.
	var agentOpt, financeOpt reqOpt
	var signedInAt time.Time
	refresh := func() {
		if agentOpt == nil || !signedInAt.Equal(e.now) {
			agentOpt, financeOpt, signedInAt = browser(e.loginCookie(identity.SeedAgentEmail, seedPassword)), e.asAdmin(devseed.FinanceAdminEmail), e.now
		}
	}
	agent := func() reqOpt { refresh(); return agentOpt }
	finance := func() reqOpt { refresh(); return financeOpt }
	support := e.asAdmin(devseed.SupportEmail)

	// The agent is offered Agent Starter (in US dollars), not the agency plan.
	rec := e.get("/v1/me/subscription-plans?market=ZW", agent())
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, "Agent Starter") || strings.Contains(body, "Agency Pro") || !strings.Contains(body, `"currency_code":"USD"`) {
		t.Fatalf("plans: %s", body)
	}
	plan := devseed.AgentStarterPlanID.String()

	// One request at a time: the seeded one waits for payment.
	wantError(t, e.post("/v1/me/subscriptions", obj{"plan_id": plan, "period": "QUARTERLY", "market_code": "ZW"}, agent()),
		http.StatusConflict, subscriptions.CodePendingExists)
	wantStatus(t, e.post("/v1/me/subscriptions/"+devseed.PendingAgentSubscription.String()+"/cancel", nil, agent()), http.StatusOK)
	wantError(t, e.post("/v1/me/subscriptions", obj{"plan_id": devseed.AgencyProPlanID.String(), "period": "YEARLY", "organisation_id": devseed.SampleEstates.ID.String()}, agent()),
		http.StatusForbidden, subscriptions.CodeNotOrganisationOwner)

	rec = e.post("/v1/me/subscriptions", obj{"plan_id": plan, "period": "YEARLY", "market_code": "ZW"}, agent())
	wantStatus(t, rec, http.StatusCreated)
	sub := decode[subscriptionBody](t, rec).Subscription
	if sub.Status != subscriptions.StatusPendingPayment || sub.Amount != "100.00" || sub.CurrencyCode != "USD" || sub.PaymentInstructions == nil ||
		!strings.HasPrefix(sub.InvoiceNumber, "RPS-2026-") {
		t.Fatalf("requested: %+v", sub)
	}
	// Another customer cannot see it.
	wantError(t, e.get("/v1/me/subscriptions/"+sub.ID, browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))),
		http.StatusNotFound, subscriptions.CodeSubscriptionNotFound)

	// SUPPORT reads but does not decide; the payment must cover the invoice.
	wantStatus(t, e.get("/v1/admin/subscriptions?status=PENDING_PAYMENT", support), http.StatusOK)
	approve := obj{"payment_reference": "BANK-778812", "amount": "100", "received_on": "2026-10-30", "note": "Bank transfer"}
	wantError(t, e.post("/v1/admin/subscriptions/"+sub.ID+"/approve", approve, support), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.post("/v1/admin/subscriptions/"+sub.ID+"/approve", obj{"payment_reference": "B-1", "amount": "99.99", "received_on": "2026-10-30"}, finance()),
		http.StatusUnprocessableEntity, subscriptions.CodePaymentTooLow)
	wantError(t, e.post("/v1/admin/subscriptions/"+sub.ID+"/approve", obj{"payment_reference": "B-1", "amount": "100", "received_on": "2027-01-01"}, finance()),
		http.StatusBadRequest, "VALIDATION_FAILED")
	rec = e.post("/v1/admin/subscriptions/"+sub.ID+"/approve", approve, finance())
	wantStatus(t, rec, http.StatusOK)
	active := decode[subscriptionBody](t, rec).Subscription
	if active.Status != subscriptions.StatusActive || active.StartsAt == nil || !active.StartsAt.Equal(e.now) || !active.EndsAt.Equal(e.now.AddDate(1, 0, 0)) {
		t.Fatalf("approved: %+v", active)
	}
	wantError(t, e.post("/v1/admin/subscriptions/"+sub.ID+"/approve", approve, finance()), http.StatusConflict, subscriptions.CodeSubscriptionState)

	// Renewal opens 30 days before the end.
	wantError(t, e.post("/v1/me/subscriptions/"+sub.ID+"/renew", nil, agent()), http.StatusConflict, subscriptions.CodeRenewalNotOpen)
	e.now = e.now.AddDate(0, 11, 5)
	rec = e.post("/v1/me/subscriptions/"+sub.ID+"/renew", obj{"period": "QUARTERLY"}, agent())
	wantStatus(t, rec, http.StatusCreated)
	renewal := decode[subscriptionBody](t, rec).Subscription
	if renewal.RenewalOf == nil || *renewal.RenewalOf != sub.ID || renewal.Amount != "30.00" {
		t.Fatalf("renewal: %+v", renewal)
	}
	rec = e.post("/v1/admin/subscriptions/"+renewal.ID+"/approve", obj{"payment_reference": "BANK-9", "amount": "30.00", "received_on": e.now.Format("2006-01-02")}, finance())
	wantStatus(t, rec, http.StatusOK)
	next := decode[subscriptionBody](t, rec).Subscription
	if !next.StartsAt.Equal(*active.EndsAt) || !next.EndsAt.Equal(active.EndsAt.AddDate(0, 3, 0)) {
		t.Fatalf("the renewal starts when the current period ends: %+v", next)
	}
	rec = e.get("/v1/me/subscriptions", agent())
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"upcoming":{`) || !strings.Contains(body, `"limit_enforced":false`) {
		t.Fatalf("mine: %s", body)
	}

	// Past the end the first one is EXPIRED at once, and the sweep records it.
	e.now = active.EndsAt.Add(time.Hour)
	rec = e.get("/v1/admin/subscriptions/"+sub.ID, finance())
	wantStatus(t, rec, http.StatusOK)
	if s := decode[subscriptionBody](t, rec).Subscription; s.Status != subscriptions.StatusExpired {
		t.Fatalf("status = %s, want EXPIRED", s.Status)
	}
	svc := subscriptions.NewService(subscriptions.Deps{Store: e.subs, Now: func() time.Time { return e.now }})
	if n, err := svc.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}

	// An administrator cancels the running renewal and records a refund is due.
	rec = e.post("/v1/admin/subscriptions/"+renewal.ID+"/cancel", obj{"reason": "Plan withdrawn early", "refund_due": true, "refund_note": "Pro rata"}, finance())
	wantStatus(t, rec, http.StatusOK)
	if s := decode[subscriptionBody](t, rec).Subscription; s.Status != subscriptions.StatusCancelled || !s.RefundDue {
		t.Fatalf("cancelled: %+v", s)
	}
	var types []string
	for _, ev := range e.subs.Outbox() {
		types = append(types, ev.Type)
	}
	for _, want := range []string{subscriptions.EventRequested, subscriptions.EventActivated, subscriptions.EventExpired, subscriptions.EventCancelled} {
		if !strings.Contains(strings.Join(types, ","), want) {
			t.Fatalf("outbox %v lacks %s", types, want)
		}
	}
}

func TestSubscriptionReject(t *testing.T) {
	e := newEnv(t)
	finance := e.asAdmin(devseed.FinanceAdminEmail)
	id := devseed.PendingAgentSubscription.String()
	wantError(t, e.post("/v1/admin/subscriptions/"+id+"/reject", obj{"reason": ""}, finance), http.StatusBadRequest, "VALIDATION_FAILED")
	rec := e.post("/v1/admin/subscriptions/"+id+"/reject", obj{"reason": "The agent's role is not approved in this market."}, finance)
	wantStatus(t, rec, http.StatusOK)
	if s := decode[subscriptionBody](t, rec).Subscription; s.Status != subscriptions.StatusRejected {
		t.Fatalf("rejected: %+v", s)
	}
	wantError(t, e.post("/v1/me/subscriptions/"+id+"/cancel", nil, browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))),
		http.StatusConflict, subscriptions.CodeSubscriptionState)
}

func TestSubscriptionPlansAdmin(t *testing.T) {
	e := newEnv(t)
	finance := e.asAdmin(devseed.FinanceAdminEmail)
	plan := obj{"name": "Host Basic", "subscriber_kind": "INDIVIDUAL", "eligible_roles": []string{"HOST"},
		"features": []obj{{"code": "MAX_ACTIVE_LISTINGS", "limit": 3}}, "benefits": []string{"Three short stays"},
		"prices": []obj{{"currency_code": "USD", "period": "QUARTERLY", "amount": "15"}, {"currency_code": "GBP", "period": "YEARLY", "amount": "40.5"}}}
	wantError(t, e.post("/v1/admin/subscription-plans", plan, e.asAdmin(devseed.ContentAdminEmail)), http.StatusForbidden, "FORBIDDEN")
	bad := obj{"name": "Bad", "subscriber_kind": "INDIVIDUAL", "eligible_roles": []string{"CONSUMER"}, "prices": []obj{{"currency_code": "usd", "period": "MONTHLY", "amount": "-1"}}}
	if env := wantError(t, e.post("/v1/admin/subscription-plans", bad, finance), http.StatusBadRequest, "VALIDATION_FAILED"); env.Error.Fields["eligible_roles"] == "" || env.Error.Fields["prices"] == "" {
		t.Fatalf("fields: %+v", env.Error.Fields)
	}
	rec := e.post("/v1/admin/subscription-plans", plan, finance)
	wantStatus(t, rec, http.StatusCreated)
	created := decode[struct {
		Plan struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Prices []struct {
				Amount string `json:"amount"`
			} `json:"prices"`
		} `json:"plan"`
	}](t, rec).Plan
	if created.Status != "ACTIVE" || len(created.Prices) != 2 || created.Prices[0].Amount != "15.00" {
		t.Fatalf("created: %+v", created)
	}
	wantStatus(t, e.patch("/v1/admin/subscription-plans/"+created.ID, obj{"name": "Host Basic 2026"}, finance), http.StatusOK)
	wantStatus(t, e.post("/v1/admin/subscription-plans/"+created.ID+"/retire", nil, finance), http.StatusOK)
	wantError(t, e.patch("/v1/admin/subscription-plans/"+created.ID, obj{"name": "Again"}, finance), http.StatusConflict, subscriptions.CodePlanRetired)
	rec = e.get("/v1/admin/subscription-plans", e.asAdmin(devseed.SupportEmail))
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "Host Basic 2026") {
		t.Fatalf("plans: %s", rec.Body)
	}
	audit := e.get("/v1/admin/audit?target_type=SUBSCRIPTION_PLAN&target_id="+created.ID, e.asAdmin(identity.SeedAdminEmail))
	wantStatus(t, audit, http.StatusOK)
	if n := decode[struct {
		Total int `json:"total"`
	}](t, audit).Total; n != 3 {
		t.Fatalf("%d audit records, want 3 (created, updated, retired)", n)
	}
}

func TestReferenceAdmin(t *testing.T) {
	e := newEnv(t)
	content := e.asAdmin(devseed.ContentAdminEmail)
	rec := e.post("/v1/admin/reference/property_types", obj{"code": "garden-flat", "label": "Garden flat", "parent_code": "residential", "sort_order": 50}, content)
	wantStatus(t, rec, http.StatusCreated)
	if !strings.Contains(e.get("/v1/reference/property_types").Body.String(), `"garden-flat"`) {
		t.Fatal("a new value is offered at once")
	}
	wantError(t, e.post("/v1/admin/reference/property_types", obj{"code": "garden-flat", "label": "Again", "parent_code": "residential"}, content),
		http.StatusConflict, "REFERENCE_ITEM_EXISTS")
	wantError(t, e.post("/v1/admin/reference/property_types", obj{"code": "Bad Code", "label": "x", "parent_code": "nowhere"}, content),
		http.StatusBadRequest, "VALIDATION_FAILED")
	wantError(t, e.post("/v1/admin/reference/account_roles", obj{"code": "astronaut", "label": "Astronaut"}, content),
		http.StatusUnprocessableEntity, "REFERENCE_SET_NOT_EXTENSIBLE")

	wantStatus(t, e.patch("/v1/admin/reference/property_types/garden-flat", obj{"label": "Garden apartment"}, content), http.StatusOK)
	wantStatus(t, e.post("/v1/admin/reference/property_types/garden-flat/deactivate", nil, content), http.StatusOK)
	if strings.Contains(e.get("/v1/reference/property_types").Body.String(), `"garden-flat"`) {
		t.Fatal("a deactivated value is no longer offered")
	}
	rec = e.get("/v1/admin/reference/property_types", content)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"code":"garden-flat","label":"Garden apartment"`) || !strings.Contains(rec.Body.String(), `"active":false`) {
		t.Fatalf("admin list: %s", rec.Body)
	}
	wantStatus(t, e.post("/v1/admin/reference/property_types/garden-flat/reactivate", nil, content), http.StatusOK)

	// A market administrator changes only its own market's rows.
	market := e.asAdmin(devseed.MarketAdminEmail)
	wantError(t, e.patch("/v1/admin/reference/property_types/garden-flat", obj{"label": "Mine"}, market), http.StatusForbidden, "REFERENCE_MARKET_FORBIDDEN")
	wantError(t, e.get("/v1/admin/reference/nothing", content), http.StatusNotFound, "REFERENCE_SET_NOT_FOUND")
	wantError(t, e.get("/v1/admin/reference/property_types", e.asAdmin(devseed.SupportEmail)), http.StatusForbidden, "FORBIDDEN")
}

func TestAdminReports(t *testing.T) {
	e := newEnv(t)
	finance := e.asAdmin(devseed.FinanceAdminEmail)
	// A paid subscription for the revenue line.
	wantStatus(t, e.post("/v1/admin/subscriptions/"+devseed.PendingAgentSubscription.String()+"/approve",
		obj{"payment_reference": "=HYPERLINK(1)", "amount": "30", "received_on": e.now.Format("2006-01-02")}, finance), http.StatusOK)

	rec := e.get("/v1/admin/reports/summary?from=2024-01-01&to=2026-11-30&market=ZW", finance)
	wantStatus(t, rec, http.StatusOK)
	sum := decode[struct {
		NewUsers struct {
			Total int `json:"total"`
		} `json:"new_users"`
		Listings struct {
			Total int `json:"total"`
		} `json:"listings"`
		Subscriptions struct {
			Total int `json:"total"`
		} `json:"subscriptions"`
		Revenue []struct {
			CurrencyCode string `json:"currency_code"`
			Total        string `json:"total"`
			Payments     int    `json:"payments"`
		} `json:"revenue"`
	}](t, rec)
	if sum.Listings.Total == 0 || sum.Subscriptions.Total != 1 || len(sum.Revenue) != 1 || sum.Revenue[0].Total != "30.00" || sum.Revenue[0].CurrencyCode != "USD" {
		t.Fatalf("summary: %s", rec.Body)
	}
	wantError(t, e.get("/v1/admin/reports/summary?from=2026-12-01&to=2026-11-01", finance), http.StatusBadRequest, "VALIDATION_FAILED")
	wantError(t, e.get("/v1/admin/reports/summary?market=QQ", finance), http.StatusBadRequest, "VALIDATION_FAILED")

	// Exports: people only for SUPER_ADMIN and PLATFORM_ADMIN.
	wantError(t, e.get("/v1/admin/reports/users.csv", finance), http.StatusForbidden, "FORBIDDEN")
	rec = e.get("/v1/admin/reports/users.csv?from=2024-01-01&to=2026-11-30&limit=2", e.asAdmin(devseed.PlatformAdminEmail))
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" || rec.Header().Get("X-Export-Truncated") != "true" {
		t.Fatalf("headers: %v", rec.Header())
	}
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(rows) != 3 || rows[0][2] != "email" {
		t.Fatalf("users.csv: %v %q", err, rec.Body)
	}
	rec = e.get("/v1/admin/reports/subscriptions.csv?from=2024-01-01&to=2026-11-30", finance)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "'=HYPERLINK(1)") {
		t.Fatalf("a formula must be neutralised: %s", rec.Body)
	}
	wantStatus(t, e.get("/v1/admin/reports/listings.csv", e.asAdmin(identity.SeedAdminEmail)), http.StatusOK)
}
