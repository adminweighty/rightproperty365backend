package propertyrequests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Replies on property requests (API 0.46.0, FR-063, BR-022, FR-064).
//
// The receiving side (whoever may work the request: the assigned member,
// the organisation's OWNER and MANAGER, the direct seller) writes to the
// customer, and the customer who sent the request while signed in answers.
// The conversation is private to the two sides; the internal notes are
// another thing and never appear in it. A professional's reply to a request
// that is NEW_ENQUIRY or ASSIGNED moves it to CONTACTED in the same
// transaction, so the customer reads RESPONDED and the lead funnel counts
// the reply as the first response.

// Message authors.
const (
	AuthorProfessional = "PROFESSIONAL"
	AuthorCustomer     = "CUSTOMER"
)

// Timeline entries and outbox events of the conversation. The timeline
// never carries a message's text; the events carry identifiers only.
const (
	TimelineReplied         = "REPLIED"
	TimelineCustomerReplied = "CUSTOMER_REPLIED"

	EventReplied         = "PROPERTY_REQUEST_REPLIED"
	EventCustomerReplied = "PROPERTY_REQUEST_CUSTOMER_REPLIED"

	// StageViaReply marks the stage move a reply makes.
	StageViaReply = "REPLY"
)

// MaxMessageLength is the longest message, in characters.
const MaxMessageLength = 2000

// Error codes and errors of the conversation.
const (
	CodeRequestClosed = "REQUEST_CLOSED"
	CodeReplyNotOpen  = "REPLY_NOT_OPEN"
)

var (
	// ErrRequestClosed: the request is CLOSED (the receiving side can reopen it).
	ErrRequestClosed = errors.New("request is closed")
	// ErrReplyNotOpen: the customer may answer only after the receiving side wrote.
	ErrReplyNotOpen = errors.New("customer reply not open yet")
)

// MessageEventTypes lists the outbox events of the conversation.
func MessageEventTypes() []string { return []string{EventReplied, EventCustomerReplied} }

// Message is one message of a request's conversation.
type Message struct {
	ID           uuid.UUID
	RequestID    uuid.UUID
	AuthorType   string
	AuthorUserID *uuid.UUID
	AuthorName   string
	Body         string
	CreatedAt    time.Time
}

// replyAdvance is the stage a professional's reply moves a request at stage
// to, or "" when it stays where it is.
func replyAdvance(stage string) string {
	if stage == StageNewEnquiry || stage == StageAssigned {
		return StageContacted
	}
	return ""
}

// messageEventPayload is the body of the conversation's outbox events.
func messageEventPayload(r StoredRequest, m Message) map[string]any {
	return map[string]any{
		"request_id": r.ID, "reference": r.Reference, "listing_id": r.ListingID, "market_id": r.MarketID,
		"message_id": m.ID, "author": m.AuthorType, "occurred_at": m.CreatedAt,
	}
}

// CanCustomerReply reports whether the customer may write now: the
// receiving side has written at least once and the request is not CLOSED.
func (r StoredRequest) CanCustomerReply() bool {
	return r.CustomerUserID != nil && r.Stage != StageClosed && r.hasProfessionalMessage()
}

// hasProfessionalMessage: the conversation has a professional message. The
// summary keeps the author of the LAST message only, so a customer message
// last means a professional one came before it (customers cannot start).
func (r StoredRequest) hasProfessionalMessage() bool {
	return r.MessageCount > 0
}

// AwaitingReply: the last message is the customer's (the receiving side owes an answer).
func (r StoredRequest) AwaitingReply() bool { return r.LastMessageAuthor == AuthorCustomer }

// UnreadReply: the last message is the receiving side's and the customer has
// not opened the request since.
func (r StoredRequest) UnreadReply() bool {
	return r.LastMessageAuthor == AuthorProfessional && r.LastMessageAt != nil &&
		(r.CustomerLastReadAt == nil || r.CustomerLastReadAt.Before(*r.LastMessageAt))
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

func newMessage(requestID uuid.UUID, author string, actor Actor, body string, at time.Time) (Message, error) {
	text, problem := cleanText(body, MaxMessageLength)
	if problem != "" {
		return Message{}, &ValidationError{Fields: map[string]string{"body": problem}}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Message{}, fmt.Errorf("generate message id: %w", err)
	}
	by := actor.UserID
	return Message{ID: id, RequestID: requestID, AuthorType: author, AuthorUserID: &by, AuthorName: actor.Name, Body: text, CreatedAt: at}, nil
}

// Reply writes a professional's message on a request the caller can see.
func (s *Service) Reply(ctx context.Context, scope InboxScope, actor Actor, id uuid.UUID, body string) (StoredRequest, Message, error) {
	m, err := newMessage(id, AuthorProfessional, actor, body, s.clock())
	if err != nil {
		return StoredRequest{}, Message{}, err
	}
	visible := seen(scope)
	r, err := s.store.AddMessage(ctx, m, func(cur StoredRequest) error {
		if err := visible(cur); err != nil {
			return err
		}
		if cur.Stage == StageClosed {
			return ErrRequestClosed
		}
		return nil
	})
	return r, m, err
}

// CustomerReply writes the customer's message on a request they sent.
func (s *Service) CustomerReply(ctx context.Context, userID uuid.UUID, name string, id uuid.UUID, body string) (StoredRequest, Message, error) {
	m, err := newMessage(id, AuthorCustomer, Actor{UserID: userID, Name: name}, body, s.clock())
	if err != nil {
		return StoredRequest{}, Message{}, err
	}
	r, err := s.store.AddMessage(ctx, m, func(cur StoredRequest) error {
		switch {
		case cur.CustomerUserID == nil || *cur.CustomerUserID != userID:
			return ErrNotFound
		case cur.Stage == StageClosed:
			return ErrRequestClosed
		case !cur.hasProfessionalMessage():
			return ErrReplyNotOpen
		}
		return nil
	})
	return r, m, err
}

// Messages returns the conversation of a request the caller already loaded
// through Received or MineOne.
func (s *Service) Messages(ctx context.Context, id uuid.UUID) ([]Message, error) {
	out, err := s.store.Messages(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	return out, nil
}

// MarkRead records that one side opened a request. A failure is logged by
// the caller and never fails the read.
func (s *Service) MarkRead(ctx context.Context, id uuid.UUID, side string) error {
	return s.store.MarkRead(ctx, id, side, s.clock())
}

// ---------------------------------------------------------------------------
// Views and handlers
// ---------------------------------------------------------------------------

// MessageView is one message as either side sees it.
type MessageView struct {
	ID uuid.UUID `json:"id"`
	// Author is PROFESSIONAL or CUSTOMER.
	Author string `json:"author"`
	// AuthorName: to the receiving side the author's name; to the customer
	// "You" for their own messages and "The agent" / "The seller" for the
	// other side's (never a person's name or contact details).
	AuthorName string    `json:"author_name"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
	// Mine: the caller wrote it.
	Mine bool `json:"mine"`
}

// professionalMessages is the conversation as the receiving side sees it.
func professionalMessages(msgs []Message, me uuid.UUID) []MessageView {
	out := make([]MessageView, 0, len(msgs))
	for _, m := range msgs {
		name := m.AuthorName
		if name == "" {
			name = "Customer"
			if m.AuthorType == AuthorProfessional {
				name = "Your team"
			}
		}
		out = append(out, MessageView{ID: m.ID, Author: m.AuthorType, AuthorName: name, Body: m.Body, CreatedAt: m.CreatedAt,
			Mine: m.AuthorType == AuthorProfessional && m.AuthorUserID != nil && *m.AuthorUserID == me})
	}
	return out
}

// OtherSideName is how the customer is told who answered: the agent for a
// request routed to an organisation, the seller for a direct seller's.
func OtherSideName(r StoredRequest) string {
	if r.Recipient != nil && r.Recipient.Type == RecipientSeller {
		return "The seller"
	}
	return "The agent"
}

// customerMessages is the conversation as the customer sees it.
func customerMessages(r StoredRequest, msgs []Message) []MessageView {
	out := make([]MessageView, 0, len(msgs))
	other := OtherSideName(r)
	for _, m := range msgs {
		v := MessageView{ID: m.ID, Author: m.AuthorType, AuthorName: other, Body: m.Body, CreatedAt: m.CreatedAt}
		if m.AuthorType == AuthorCustomer {
			v.AuthorName, v.Mine = "You", true
		}
		out = append(out, v)
	}
	return out
}

func (h *Handler) failMessage(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrRequestClosed):
		httpx.WriteError(w, r, http.StatusConflict, CodeRequestClosed, "This request is closed. Reopen it to reply.", nil)
	case errors.Is(err, ErrReplyNotOpen):
		httpx.WriteError(w, r, http.StatusConflict, CodeReplyNotOpen, "You can reply once the agent has written to you.", nil)
	default:
		h.fail(w, r, err)
	}
}

type messageBody struct {
	Body string `json:"body"`
}

// ProfessionalMessages serves GET /v1/agent/property-requests/{id}/messages.
func (h *Handler) ProfessionalMessages(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := h.receiver(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	if _, err := h.svc.Received(r.Context(), scope, id); err != nil {
		h.fail(w, r, err)
		return
	}
	msgs, err := h.svc.Messages(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": professionalMessages(msgs, p.UserID)})
}

// PostProfessionalMessage serves POST /v1/agent/property-requests/{id}/messages.
// It answers 201 with the message and the request as it is now.
func (h *Handler) PostProfessionalMessage(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := h.receiver(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	var in messageBody
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	req, _, err := h.svc.Reply(r.Context(), scope, Actor{UserID: p.UserID, Name: p.Name}, id, in.Body)
	if err != nil {
		h.failMessage(w, r, err)
		return
	}
	// Identifiers only: never the text.
	h.logger.InfoContext(r.Context(), "property request replied",
		slog.String("property_request_id", req.ID.String()), slog.String("stage", req.Stage))
	h.writeDetailFor(w, r, p, scope, http.StatusCreated, req)
}

// CustomerMessages serves GET /v1/me/property-requests/{id}/messages.
func (h *Handler) CustomerMessages(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		unauthenticated(w, r)
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	req, _, err := h.svc.MineOne(r.Context(), p.UserID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	msgs, err := h.svc.Messages(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": customerMessages(req, msgs), "can_reply": req.CanCustomerReply()})
}

// PostCustomerMessage serves POST /v1/me/property-requests/{id}/messages.
// It answers 201 with the request as its customer sees it.
func (h *Handler) PostCustomerMessage(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		unauthenticated(w, r)
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	var in messageBody
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if _, _, err := h.svc.CustomerReply(r.Context(), p.UserID, p.Name, id, in.Body); err != nil {
		h.failMessage(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "property request customer replied", slog.String("property_request_id", id.String()))
	h.writeCustomerDetail(w, r, p.UserID, id, http.StatusCreated)
}
