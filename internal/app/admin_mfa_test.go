package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/secretbox"
	"rightproperty365.com/backend/internal/platform/totp"
)

// Administrator MFA (API 0.40.0, FR-001).

type mfaStatusBody struct {
	Enrolled               bool       `json:"enrolled"`
	Method                 *string    `json:"method"`
	CreatedAt              *time.Time `json:"created_at"`
	RecoveryCodesRemaining int        `json:"recovery_codes_remaining"`
	Required               bool       `json:"required"`
	PendingSetup           bool       `json:"pending_setup"`
	WebAuthnAvailable      bool       `json:"webauthn_available"`
}

type totpSetupBody struct {
	SecretBase32 string `json:"secret_base32"`
	OTPAuthURI   string `json:"otpauth_uri"`
	Issuer       string `json:"issuer"`
	AccountName  string `json:"account_name"`
	Digits       int    `json:"digits"`
	Period       int    `json:"period"`
}

type recoveryCodesBody struct {
	RecoveryCodes []string      `json:"recovery_codes"`
	MFA           mfaStatusBody `json:"mfa"`
}

type mfaChallengeBody struct {
	Status      string   `json:"status"`
	ChallengeID string   `json:"challenge_id"`
	Methods     []string `json:"methods"`
	CodeLength  int      `json:"code_length"`
}

type adminSessionBody struct {
	User    userBody `json:"user"`
	Session struct {
		Scope                string `json:"scope"`
		MFAEnrolmentRequired bool   `json:"mfa_enrolment_required"`
	} `json:"session"`
}

// ipSeq gives every sign-in its own address: the password step allows 5 per
// minute and IP.
var ipSeq int

func nextIP() reqOpt {
	ipSeq++
	return withIP(fmt.Sprintf("10.77.%d.%d", ipSeq/250, ipSeq%250+1))
}

// adminPasswordAndCode runs the password and emailed-code steps of the
// administrator sign-in and returns the answer of the second.
func (e *env) adminPasswordAndCode(email string, ip reqOpt) *httptest.ResponseRecorder {
	return e.post("/v1/admin/auth/login", obj{"email": email, "password": seedPassword}, ip)
}

// adminMFAChallenge signs an ENROLLED administrator in up to the MFA step.
func (e *env) adminMFAChallenge(t *testing.T, email string, ip reqOpt) string {
	t.Helper()
	rec := e.adminPasswordAndCode(email, ip)
	wantStatus(t, rec, http.StatusOK)
	if sessionCookie(rec) != nil {
		t.Fatal("an enrolled administrator got a session before the second factor")
	}
	c := decode[mfaChallengeBody](t, rec)
	if c.Status != "MFA_REQUIRED" || c.ChallengeID == "" || c.CodeLength != 6 || strings.Join(c.Methods, ",") != "totp,recovery_code" {
		t.Fatalf("unexpected challenge: %s", rec.Body)
	}
	return c.ChallengeID
}

// nextStep moves the clock to a new 30-second step, so a fresh code is not a replay.
func (e *env) nextStep() { e.now = e.now.Add(totp.Period) }

func (e *env) code(secret []byte) string { return totp.Code(secret, totp.Step(e.now)) }

// enrol sets up and confirms an authenticator for the signed-in
// administrator and returns its secret and recovery codes.
func (e *env) enrol(t *testing.T, admin reqOpt, email string) ([]byte, []string) {
	t.Helper()
	rec := e.post("/v1/admin/auth/mfa/totp/setup", nil, admin)
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the setup answer must not be cached")
	}
	setup := decode[totpSetupBody](t, rec)
	if setup.Issuer != "RightProperty365" || setup.AccountName != email || setup.Digits != 6 || setup.Period != 30 ||
		!strings.HasPrefix(setup.OTPAuthURI, "otpauth://totp/RightProperty365:"+url.PathEscape(email)+"?secret="+setup.SecretBase32+"&issuer=RightProperty365") {
		t.Fatalf("unexpected setup: %s", rec.Body)
	}
	secret, err := totp.DecodeSecret(setup.SecretBase32)
	if err != nil {
		t.Fatal(err)
	}
	st := decode[mfaStatusBody](t, e.get("/v1/admin/auth/mfa", admin))
	if st.Enrolled || !st.PendingSetup {
		t.Fatalf("pending status = %+v", st)
	}
	e.nextStep()
	rec = e.post("/v1/admin/auth/mfa/totp/confirm", obj{"code": e.code(secret)}, admin)
	wantStatus(t, rec, http.StatusOK)
	body := decode[recoveryCodesBody](t, rec)
	if len(body.RecoveryCodes) != 10 || !body.MFA.Enrolled || body.MFA.RecoveryCodesRemaining != 10 || *body.MFA.Method != "totp" {
		t.Fatalf("unexpected confirm: %s", rec.Body)
	}
	return secret, body.RecoveryCodes
}

func (e *env) auditActions(target uuid.UUID) []adminaudit.Event {
	var out []adminaudit.Event
	for _, ev := range e.audit.All() {
		if ev.TargetID == target.String() && strings.HasPrefix(ev.Action, "ADMIN_MFA_") {
			out = append(out, ev)
		}
	}
	return out
}

func (e *env) mfaEvents(typ string, user uuid.UUID) int {
	n := 0
	for _, ev := range e.identity.Outbox() {
		if ev.Type == typ && ev.AggregateID == user {
			n++
		}
	}
	return n
}

// The whole life of an authenticator for an administrator while MFA is not
// required: setup, confirm, the three-step sign-in with a TOTP code and
// with a recovery code (once only), replay refusal, regeneration, removal —
// each audited, and no secret or code ever in the logs.
func TestAdminMFAEnrolmentAndSignIn(t *testing.T) {
	e := newEnv(t)
	email := devseed.PlatformAdminEmail
	admin := browser(e.adminLogin(email, seedPassword, nextIP()))

	st := decode[mfaStatusBody](t, e.get("/v1/admin/auth/mfa", admin))
	if st.Enrolled || st.Method != nil || st.Required || st.WebAuthnAvailable || st.RecoveryCodesRemaining != 0 {
		t.Fatalf("initial status = %+v", st)
	}
	// Confirm without a setup.
	wantError(t, e.post("/v1/admin/auth/mfa/totp/confirm", obj{"code": "123456"}, admin), http.StatusConflict, identity.CodeMFASetupRequired)

	// A wrong code does not confirm.
	rec := e.post("/v1/admin/auth/mfa/totp/setup", nil, admin)
	wantStatus(t, rec, http.StatusOK)
	secret0, _ := totp.DecodeSecret(decode[totpSetupBody](t, rec).SecretBase32)
	wrong := fmt.Sprintf("%06d", (atoi(t, e.code(secret0))+1)%1000000)
	env := wantError(t, e.post("/v1/admin/auth/mfa/totp/confirm", obj{"code": wrong}, admin), http.StatusBadRequest, "VALIDATION_FAILED")
	if env.Error.Fields["code"] == "" {
		t.Fatal("no field error")
	}
	// A new setup replaces the pending one.
	secret, recovery := e.enrol(t, admin, email)
	if string(secret) == string(secret0) {
		t.Fatal("setup did not replace the pending secret")
	}
	wantError(t, e.post("/v1/admin/auth/mfa/totp/setup", nil, admin), http.StatusConflict, identity.CodeMFAAlreadyEnrolled)
	if acts := e.auditActions(devseed.PlatformAdminID); len(acts) != 1 || acts[0].Action != adminaudit.ActionAdminMFAEnrolled ||
		acts[0].ActorUserID == nil || *acts[0].ActorUserID != devseed.PlatformAdminID {
		t.Fatalf("audit = %+v", acts)
	}

	// Sign in: password, emailed code, then the authenticator.
	ip := nextIP()
	challenge := e.adminMFAChallenge(t, email, ip)
	// The emailed-code step is over: neither verify nor resend take it now.
	wantError(t, e.post("/v1/admin/auth/login/verify", obj{"challenge_id": challenge, "code": "000000"}, ip), http.StatusBadRequest, "CODE_EXPIRED")
	wantError(t, e.post("/v1/admin/auth/login/resend", obj{"challenge_id": challenge}, ip), http.StatusBadRequest, "CODE_EXPIRED")
	// Validation: neither or both answers, malformed codes (not counted).
	wantError(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge}, ip), http.StatusBadRequest, "VALIDATION_FAILED")
	wantError(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "code": "123456", "recovery_code": recovery[0]}, ip),
		http.StatusBadRequest, "VALIDATION_FAILED")
	wantError(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "code": "12345"}, ip), http.StatusBadRequest, "VALIDATION_FAILED")
	// The code used to confirm is refused: same step (replay).
	env = wantError(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "code": e.code(secret)}, ip), http.StatusBadRequest, "VALIDATION_FAILED")
	if !strings.Contains(env.Error.Fields["code"], "already used") {
		t.Fatalf("replay message = %q", env.Error.Fields["code"])
	}
	// The previous step's code is within tolerance but older than the last
	// used step: also refused.
	e.nextStep()
	wantError(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "code": totp.Code(secret, totp.Step(e.now)-1)}, ip),
		http.StatusBadRequest, "VALIDATION_FAILED")
	totpCode := e.code(secret)
	rec = e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "code": totpCode}, ip)
	wantStatus(t, rec, http.StatusOK)
	sess := decode[adminSessionBody](t, rec)
	cookie := sessionCookie(rec)
	if cookie == nil || sess.Session.Scope != "admin" || sess.Session.MFAEnrolmentRequired || sess.User.Email != email {
		t.Fatalf("unexpected session answer: %s", rec.Body)
	}
	wantStatus(t, e.get("/v1/admin/me", browser(cookie)), http.StatusOK)
	// The challenge is used up.
	wantError(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "code": totpCode}, ip), http.StatusBadRequest, "CODE_EXPIRED")

	// A recovery code works once (any case, with or without the dash), and
	// emails a notice.
	ip = nextIP()
	challenge = e.adminMFAChallenge(t, email, ip)
	rec = e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "recovery_code": strings.ToUpper(recovery[0])}, ip)
	wantStatus(t, rec, http.StatusOK)
	if sessionCookie(rec) == nil {
		t.Fatal("no session after a recovery code")
	}
	if e.mfaEvents(identity.EventAdminMFARecoveryCodeUsed, devseed.PlatformAdminID) != 1 {
		t.Fatal("no recovery code notice")
	}
	ip = nextIP()
	challenge = e.adminMFAChallenge(t, email, ip)
	wantError(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "recovery_code": strings.ReplaceAll(recovery[0], "-", "")}, ip),
		http.StatusBadRequest, "VALIDATION_FAILED")
	st = decode[mfaStatusBody](t, e.get("/v1/admin/auth/mfa", admin))
	if !st.Enrolled || st.RecoveryCodesRemaining != 9 || st.CreatedAt == nil {
		t.Fatalf("status after a recovery code = %+v", st)
	}

	// Regenerating needs a current code and voids the old codes.
	wantError(t, e.post("/v1/admin/auth/mfa/recovery-codes/regenerate", obj{"code": wrong}, admin), http.StatusBadRequest, "VALIDATION_FAILED")
	e.nextStep()
	rec = e.post("/v1/admin/auth/mfa/recovery-codes/regenerate", obj{"code": e.code(secret)}, admin)
	wantStatus(t, rec, http.StatusOK)
	fresh := decode[recoveryCodesBody](t, rec)
	if len(fresh.RecoveryCodes) != 10 || fresh.MFA.RecoveryCodesRemaining != 10 {
		t.Fatalf("regenerate = %s", rec.Body)
	}
	wantError(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "recovery_code": recovery[1]}, ip), http.StatusBadRequest, "VALIDATION_FAILED")
	wantStatus(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "recovery_code": fresh.RecoveryCodes[3]}, ip), http.StatusOK)

	// Removal (MFA not required) needs a current code.
	e.nextStep()
	rec = e.do(http.MethodDelete, "/v1/admin/auth/mfa/totp", obj{"code": e.code(secret)}, admin)
	wantStatus(t, rec, http.StatusNoContent)
	if st := decode[mfaStatusBody](t, e.get("/v1/admin/auth/mfa", admin)); st.Enrolled || st.RecoveryCodesRemaining != 0 {
		t.Fatalf("status after removal = %+v", st)
	}
	// The next sign-in is two steps again.
	if c := e.adminLogin(email, seedPassword, nextIP()); c == nil {
		t.Fatal("no session")
	}

	var actions []string
	for _, ev := range e.auditActions(devseed.PlatformAdminID) {
		actions = append(actions, ev.Action)
	}
	for _, want := range []string{adminaudit.ActionAdminMFAEnrolled, adminaudit.ActionAdminMFARecoveryCodeUsed,
		adminaudit.ActionAdminMFARecoveryRegenerated, adminaudit.ActionAdminMFARemoved} {
		if !strings.Contains(strings.Join(actions, ","), want) {
			t.Errorf("no %s audit record in %v", want, actions)
		}
	}

	// Nothing secret reached the logs: the secret, the URI, the codes, the
	// address (only its fingerprint).
	logs := e.logs.String()
	if !strings.Contains(logs, `"event":"admin_mfa_sign_in"`) || !strings.Contains(logs, `"mfa_method":"recovery_code"`) {
		t.Fatal("the MFA steps were not audited in the log")
	}
	for _, s := range append(append([]string{totp.EncodeSecret(secret), totp.EncodeSecret(secret0), "otpauth://", email,
		`"` + totpCode + `"`, "=" + totpCode}, recovery...), fresh.RecoveryCodes...) {
		if strings.Contains(logs, s) || strings.Contains(logs, strings.ReplaceAll(s, "-", "")) {
			t.Fatalf("the logs contain %q", s)
		}
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// With ADMIN_MFA_REQUIRED an administrator without an authenticator gets an
// enrolment-only session: every other administrator route is 403
// MFA_ENROLMENT_REQUIRED until confirm, which lifts it on the same session.
func TestAdminMFARequiredGivesAnEnrolmentOnlySession(t *testing.T) {
	e := newEnvWithConfig(t, func(c *config.Config) { c.AdminMFARequired = true })
	email := devseed.PlatformAdminEmail
	rec := e.adminPasswordAndCode(email, nextIP())
	wantStatus(t, rec, http.StatusOK)
	if !decode[adminSessionBody](t, rec).Session.MFAEnrolmentRequired {
		t.Fatalf("not marked enrolment-only: %s", rec.Body)
	}
	admin := browser(sessionCookie(rec))
	for _, path := range []string{"/v1/admin/me", "/v1/admin/users", "/v1/admin/overview", "/v1/admin/audit"} {
		wantError(t, e.get(path, admin), http.StatusForbidden, "MFA_ENROLMENT_REQUIRED")
	}
	wantError(t, e.post("/v1/admin/users/"+devseed.SupportID.String()+"/suspend", obj{"reason": "Testing the gate"}, admin),
		http.StatusForbidden, "MFA_ENROLMENT_REQUIRED")
	wantError(t, e.post("/v1/admin/auth/mfa/recovery-codes/regenerate", obj{"code": "123456"}, admin), http.StatusForbidden, "MFA_ENROLMENT_REQUIRED")
	st := decode[mfaStatusBody](t, e.get("/v1/admin/auth/mfa", admin))
	if st.Enrolled || !st.Required {
		t.Fatalf("status = %+v", st)
	}

	secret, _ := e.enrol(t, admin, email)
	// The same session is now a full administrator session.
	wantStatus(t, e.get("/v1/admin/me", admin), http.StatusOK)
	wantStatus(t, e.get("/v1/admin/users", admin), http.StatusOK)
	// Removal is refused while MFA is required.
	e.nextStep()
	wantError(t, e.do(http.MethodDelete, "/v1/admin/auth/mfa/totp", obj{"code": e.code(secret)}, admin), http.StatusForbidden, identity.CodeMFARemovalNotAllowed)

	// Another administrator, signed in to an enrolment-only session, can sign out.
	rec = e.adminPasswordAndCode(devseed.SupportEmail, nextIP())
	other := browser(sessionCookie(rec))
	wantStatus(t, e.post("/v1/auth/logout", nil, other), http.StatusNoContent)
	if e.get("/v1/admin/auth/mfa", other).Code != http.StatusUnauthorized {
		t.Fatal("the session survived sign-out")
	}
	// Customers are not affected.
	wantStatus(t, e.get("/v1/me", browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))), http.StatusOK)
}

// The seeded MFA administrator (fixed development secret): five wrong
// second-factor answers lock the challenge, once audited; then a fresh
// sign-in works with the seeded secret and a seeded recovery code.
func TestAdminMFALockoutAndSeededAdministrator(t *testing.T) {
	e := newEnv(t)
	box, err := secretbox.New(secretbox.DevelopmentKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := devseed.SeedMemoryAdminMFA(e.identity, box, identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}, e.now); err != nil {
		t.Fatal(err)
	}
	secret, err := totp.DecodeSecret(devseed.DevMFASecretBase32)
	if err != nil {
		t.Fatal(err)
	}
	right := e.code(secret)
	wrong := fmt.Sprintf("%06d", (atoi(t, right)+500000)%1000000)

	ip := nextIP()
	challenge := e.adminMFAChallenge(t, devseed.MFAAdminEmail, ip)
	for i := 1; i <= 4; i++ {
		body := obj{"challenge_id": challenge, "code": wrong}
		if i%2 == 0 {
			body = obj{"challenge_id": challenge, "recovery_code": "zzzzz-zzzzz"}
		}
		wantError(t, e.post("/v1/admin/auth/login/mfa", body, ip), http.StatusBadRequest, "VALIDATION_FAILED")
	}
	wantError(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "code": wrong}, ip), http.StatusBadRequest, "CODE_EXPIRED")
	// Locked: even the right code is refused now.
	wantError(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "code": right}, ip), http.StatusBadRequest, "CODE_EXPIRED")
	locks := 0
	for _, ev := range e.auditActions(devseed.MFAAdminID) {
		if ev.Action == adminaudit.ActionAdminMFAChallengeLocked {
			locks++
			if ev.After["failed_attempts"] != 5 {
				t.Fatalf("lock record = %+v", ev.After)
			}
		}
	}
	if locks != 1 {
		t.Fatalf("%d lock records, want 1", locks)
	}

	ip = nextIP()
	challenge = e.adminMFAChallenge(t, devseed.MFAAdminEmail, ip)
	wantStatus(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "code": right}, ip), http.StatusOK)
	ip = nextIP()
	challenge = e.adminMFAChallenge(t, devseed.MFAAdminEmail, ip)
	wantStatus(t, e.post("/v1/admin/auth/login/mfa", obj{"challenge_id": challenge, "recovery_code": "DEVRC-AAAA2"}, ip), http.StatusOK)
}

// A SUPER_ADMIN resets another administrator's MFA with a reason: audited,
// emailed, every session of the target revoked. Nobody else can, and not
// on their own account.
func TestAdminMFASuperAdminReset(t *testing.T) {
	e := newEnv(t)
	email := devseed.PlatformAdminEmail
	patCookie := e.adminLogin(email, seedPassword, nextIP())
	pat := browser(patCookie)
	super := browser(e.adminLogin(identity.SeedAdminEmail, seedPassword, nextIP()))
	path := "/v1/admin/users/" + devseed.PlatformAdminID.String() + "/mfa"

	wantError(t, e.post(path+"/reset", obj{"reason": "Lost phone"}, super), http.StatusConflict, identity.CodeMFANotEnrolled)
	e.enrol(t, pat, email)
	if st := decode[mfaStatusBody](t, e.get(path, super)); !st.Enrolled {
		t.Fatalf("status = %+v", st)
	}
	// Only a SUPER_ADMIN; never one's own; a reason is required.
	wantError(t, e.get(path, pat), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.post("/v1/admin/users/"+identity.SeedAdminUserID.String()+"/mfa/reset", obj{"reason": "Lost phone"}, pat), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.post("/v1/admin/users/"+identity.SeedAdminUserID.String()+"/mfa/reset", obj{"reason": "Lost phone"}, super), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.post(path+"/reset", obj{"reason": " "}, super), http.StatusBadRequest, "VALIDATION_FAILED")
	wantError(t, e.post("/v1/admin/users/"+uuid.NewString()+"/mfa/reset", obj{"reason": "Lost phone"}, super), http.StatusNotFound, "NOT_FOUND")

	rec := e.post(path+"/reset", obj{"reason": "Lost phone, identity checked by call"}, super)
	wantStatus(t, rec, http.StatusOK)
	if st := decode[mfaStatusBody](t, rec); st.Enrolled {
		t.Fatalf("still enrolled: %s", rec.Body)
	}
	// Signed out everywhere.
	wantStatus(t, e.get("/v1/admin/me", pat), http.StatusUnauthorized)
	var reset *adminaudit.Event
	for _, ev := range e.auditActions(devseed.PlatformAdminID) {
		if ev.Action == adminaudit.ActionAdminMFAReset {
			reset = &ev
		}
	}
	if reset == nil || reset.Reason != "Lost phone, identity checked by call" || reset.ActorUserID == nil || *reset.ActorUserID != identity.SeedAdminUserID {
		t.Fatalf("reset audit = %+v", reset)
	}
	if e.mfaEvents(identity.EventAdminMFAReset, devseed.PlatformAdminID) != 1 {
		t.Fatal("the administrator was not notified")
	}
	// The administrators' audit trail shows it.
	if body := e.get("/v1/admin/audit?action=ADMIN_MFA_RESET", super).Body.String(); !strings.Contains(body, "ADMIN_MFA_RESET") {
		t.Fatalf("audit trail: %s", body)
	}
	// The next sign-in is two steps again.
	e.adminLogin(email, seedPassword, nextIP())
}

// Deleting an administrator's account removes their MFA rows.
func TestAdminMFAAccountDeletion(t *testing.T) {
	e := newEnv(t)
	pat := browser(e.adminLogin(devseed.PlatformAdminEmail, seedPassword, nextIP()))
	e.enrol(t, pat, devseed.PlatformAdminEmail)
	if !e.identity.HasAdminMFA(devseed.PlatformAdminID) {
		t.Fatal("not enrolled")
	}
	super := browser(e.adminLogin(identity.SeedAdminEmail, seedPassword, nextIP()))
	wantStatus(t, e.post("/v1/admin/users/"+devseed.PlatformAdminID.String()+"/delete", obj{"reason": "Left the company"}, super), http.StatusOK)
	if e.identity.HasAdminMFA(devseed.PlatformAdminID) {
		t.Fatal("the MFA rows survived the account deletion")
	}
}
