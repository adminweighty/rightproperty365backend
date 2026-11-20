package app

import (
	"net/http"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// The conversation on a property request (API 0.46.0) through the full
// router: who may write on each side, the stage move of the first reply,
// the customer's view (no notes, no names) and the read markers.
func TestRequestConversation(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	tendai, rudo, chipo, blake := c.as(identity.SeedAgentEmail), c.as(devseed.EstatesOwnerEmail), c.as(devseed.EstatesAgent2Email), c.as(identity.SeedAgentBEmail)
	consumer := c.as(identity.SeedConsumerEmail)

	id := e.sent(asCustomer(e.propertyRequest(1)), consumer)
	agentPath := "/v1/agent/property-requests/" + id + "/messages"
	minePath := "/v1/me/property-requests/" + id

	// Nobody but the receiving side writes as it; the customer cannot start.
	wantError(t, e.post(agentPath, obj{"body": "Hi"}), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	for _, who := range []reqOpt{blake, chipo} {
		wantError(t, e.post(agentPath, obj{"body": "Hi"}, who), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
		wantError(t, e.get(agentPath, who), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
	}
	wantError(t, e.post(minePath+"/messages", obj{"body": "Hello?"}, consumer), http.StatusConflict, propertyrequests.CodeReplyNotOpen)
	wantError(t, e.post(minePath+"/messages", obj{"body": "Hello?"}, tendai), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
	wantError(t, e.post(agentPath, obj{"body": ""}, tendai), http.StatusBadRequest, httpx.CodeValidationFailed)

	// A private note stays private.
	wantStatus(t, e.post("/v1/agent/property-requests/"+id+"/notes", obj{"body": internalNote}, rudo), http.StatusCreated)

	// The assigned agent replies: 201 with the request, now CONTACTED.
	rec := e.post(agentPath, obj{"body": "Yes, it is available. Saturday at ten?"}, tendai)
	wantStatus(t, rec, http.StatusCreated)
	r := requestOf(t, rec)
	msgs := list(r, "messages")
	if r["stage"] != "CONTACTED" || r["message_count"] != float64(1) || r["awaiting_reply"] != false || len(msgs) != 1 ||
		msgs[0].(obj)["mine"] != true || r["can_reply"] != true {
		t.Fatalf("after the reply: %s", rec.Body)
	}

	// The customer sees it as "The agent", with no note and no name.
	list1 := decode[obj](t, e.get("/v1/me/property-requests", consumer))
	row := list(list1, "items")[0].(obj)
	if row["unread_reply"] != true || row["message_count"] != float64(1) || row["status"] != "RESPONDED" {
		t.Fatalf("customer row: %v", row)
	}
	rec = e.get(minePath, consumer)
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	if strings.Contains(body, internalNote) || strings.Contains(body, "Tendai") {
		t.Fatalf("customer view leaks a note or a name: %s", body)
	}
	mine := requestOf(t, rec)
	cm := list(mine, "messages")
	if len(cm) != 1 || cm[0].(obj)["author_name"] != "The agent" || mine["can_reply"] != true {
		t.Fatalf("customer detail: %s", body)
	}
	row = list(decode[obj](t, e.get("/v1/me/property-requests", consumer)), "items")[0].(obj)
	if row["unread_reply"] != false {
		t.Fatalf("opening the request did not mark the reply read: %v", row)
	}

	// The customer answers; the agent's inbox shows it waits for a reply.
	rec = e.post(minePath+"/messages", obj{"body": "Ten is fine. Is there parking?"}, consumer)
	wantStatus(t, rec, http.StatusCreated)
	if got := list(requestOf(t, rec), "messages"); len(got) != 2 || got[1].(obj)["author_name"] != "You" || got[1].(obj)["mine"] != true {
		t.Fatalf("customer answer: %s", rec.Body)
	}
	inbox := decode[obj](t, e.get("/v1/agent/property-requests", tendai))
	found := false
	for _, it := range list(inbox, "items") {
		if it.(obj)["request_id"] == id {
			found = it.(obj)["awaiting_reply"] == true && it.(obj)["message_count"] == float64(2)
		}
	}
	if !found {
		t.Fatalf("inbox row: %v", inbox)
	}
	// The owner (who may work the request) can answer too.
	wantStatus(t, e.post(agentPath, obj{"body": "Yes, two bays."}, rudo), http.StatusCreated)
	got := decode[obj](t, e.get(agentPath, tendai))
	if items := list(got, "items"); len(items) != 3 || items[2].(obj)["mine"] != false {
		t.Fatalf("agent messages: %v", got)
	}

	// Closed: neither side writes.
	wantStatus(t, e.patch("/v1/agent/property-requests/"+id, obj{"stage": "CLOSED"}, tendai), http.StatusOK)
	wantError(t, e.post(agentPath, obj{"body": "One more thing"}, tendai), http.StatusConflict, propertyrequests.CodeRequestClosed)
	wantError(t, e.post(minePath+"/messages", obj{"body": "Thanks"}, consumer), http.StatusConflict, propertyrequests.CodeRequestClosed)
	if mine := requestOf(t, e.get(minePath, consumer)); mine["can_reply"] != false {
		t.Fatalf("closed can_reply: %v", mine["can_reply"])
	}
}
