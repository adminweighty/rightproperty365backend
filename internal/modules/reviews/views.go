package reviews

import (
	"time"

	"github.com/google/uuid"
)

// The JSON of the 0.36 contract's "Response shapes (e)".

// ProfileRef is the reviewed profile.
type ProfileRef struct {
	ID   uuid.UUID `json:"id"`
	Kind string    `json:"kind"`
	Slug string    `json:"slug"`
	Name string    `json:"name"`
}

func profileRef(p Profile) ProfileRef {
	return ProfileRef{ID: p.ID, Kind: p.Kind, Slug: p.Slug, Name: p.Name}
}

func reviewProfile(rv Review) ProfileRef {
	return ProfileRef{ID: rv.ProfileID, Kind: rv.ProviderKind, Slug: rv.ProviderSlug, Name: rv.ProviderName}
}

// BasisView is a basis as the reviewer sees it.
type BasisView struct {
	Kind      string    `json:"kind"`
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	Label     string    `json:"label"`
	// Date is when the job was completed, or when the enquiry was sent.
	Date *time.Time `json:"date"`
}

func basisView(b basis) BasisView {
	v := BasisView{Kind: b.Kind, ID: b.ID, Reference: b.Reference, Label: b.Label}
	if !b.Date.IsZero() {
		d := b.Date
		v.Date = &d
	}
	return v
}

// UpcomingBasis is a basis that does not allow a review yet.
type UpcomingBasis struct {
	Basis        BasisView  `json:"basis"`
	Reason       string     `json:"reason"`
	EligibleFrom *time.Time `json:"eligible_from"`
}

// ReviewedBasis is a basis that already has a review.
type ReviewedBasis struct {
	Basis    BasisView  `json:"basis"`
	ReviewID *uuid.UUID `json:"review_id"`
	Status   string     `json:"status"`
}

// BasisRef is a review's basis.
type BasisRef struct {
	Kind      string    `json:"kind"`
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
}

// PersonRef names a person.
type PersonRef struct {
	UserID *uuid.UUID `json:"user_id"`
	Name   string     `json:"name"`
}

// ReviewerView is the reviewer as the public sees them.
type ReviewerView struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// publicKind is CUSTOMER for any individual (a direct owner included).
func publicKind(kind string) string {
	if kind == ReviewerOrganisation {
		return ReviewerOrganisation
	}
	return ReviewerCustomer
}

func reviewerView(rv Review) ReviewerView {
	return ReviewerView{Kind: publicKind(rv.ReviewerKind), Name: rv.ReviewerName}
}

// ReplyView is the provider's reply.
type ReplyView struct {
	Text      string    `json:"text"`
	RepliedAt time.Time `json:"replied_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func replyView(rv Review) *ReplyView {
	if rv.Reply == nil {
		return nil
	}
	return &ReplyView{Text: rv.Reply.Text, RepliedAt: rv.Reply.At, UpdatedAt: rv.Reply.UpdatedAt}
}

// RatingView is a profile's published rating.
type RatingView struct {
	Average *string `json:"average"`
	Count   int     `json:"count"`
}

// RatingOf is the view of a Rating.
func RatingOf(r Rating) RatingView { return RatingView{Average: Average(r), Count: r.Count} }

// ModerationView is the last moderation as the reviewer sees it.
type ModerationView struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// MyReview is a review as its reviewer sees it (/v1/me/reviews and the
// book's /reviews).
type MyReview struct {
	ID          uuid.UUID       `json:"id"`
	Profile     ProfileRef      `json:"profile"`
	Basis       BasisRef        `json:"basis"`
	Rating      int             `json:"rating"`
	Text        string          `json:"text"`
	Status      string          `json:"status"`
	StatusLabel string          `json:"status_label"`
	Reviewer    ReviewerView    `json:"reviewer"`
	Author      *PersonRef      `json:"author"`
	Moderation  *ModerationView `json:"moderation"`
	Reply       *ReplyView      `json:"reply"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	PublishedAt *time.Time      `json:"published_at"`
	Can         struct {
		Edit   bool `json:"edit"`
		Delete bool `json:"delete"`
	} `json:"can"`
}

// MyView builds a MyReview; write is the caller's write access.
func MyView(rv Review, write bool) MyReview {
	v := MyReview{ID: rv.ID, Profile: reviewProfile(rv), Basis: BasisRef{Kind: rv.BasisKind, ID: rv.BasisID, Reference: rv.BasisReference},
		Rating: rv.Rating, Text: rv.Text, Status: rv.Status, StatusLabel: StatusLabel(rv.Status), Reviewer: reviewerView(rv),
		Reply: replyView(rv), CreatedAt: rv.CreatedAt, UpdatedAt: rv.UpdatedAt, PublishedAt: rv.PublishedAt}
	if rv.ReviewerKind == ReviewerOrganisation {
		v.Author = &PersonRef{UserID: rv.ReviewerUserID, Name: rv.AuthorName}
	}
	if rv.ModeratedAt != nil {
		v.Moderation = &ModerationView{Reason: rv.ModerationReason, At: *rv.ModeratedAt}
	}
	v.Can.Edit = write && rv.Status == StatusPending
	v.Can.Delete = write
	return v
}

// ReceivedReview is a published review as its provider sees it.
type ReceivedReview struct {
	ID                 uuid.UUID    `json:"id"`
	Profile            ProfileRef   `json:"profile"`
	Basis              ReceivedRef  `json:"basis"`
	Rating             int          `json:"rating"`
	Text               string       `json:"text"`
	Reviewer           ReviewerView `json:"reviewer"`
	PublishedAt        *time.Time   `json:"published_at"`
	Reply              *ReplyView   `json:"reply"`
	ReplyRemoved       bool         `json:"reply_removed"`
	ReplyEditableUntil *time.Time   `json:"reply_editable_until"`
	Can                struct {
		Reply     bool `json:"reply"`
		EditReply bool `json:"edit_reply"`
	} `json:"can"`
}

// ReceivedRef is the basis as the provider sees it.
type ReceivedRef struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
}

// ReceivedView builds a ReceivedReview; mayReply is OWNER or MANAGER.
func ReceivedView(rv Review, mayReply bool, now time.Time) ReceivedReview {
	v := ReceivedReview{ID: rv.ID, Profile: reviewProfile(rv), Basis: ReceivedRef{Kind: rv.BasisKind, Reference: rv.BasisReference},
		Rating: rv.Rating, Text: rv.Text, Reviewer: reviewerView(rv), PublishedAt: rv.PublishedAt, Reply: replyView(rv),
		ReplyRemoved: rv.ReplyRemovedAt != nil}
	if rv.Reply != nil {
		until := ReplyEditableUntil(*rv.Reply)
		v.ReplyEditableUntil = &until
		v.Can.EditReply = mayReply && now.Before(until)
	}
	v.Can.Reply = mayReply && rv.Reply == nil && rv.ReplyRemovedAt == nil
	return v
}

// PublicReview is a published review on the directory.
type PublicReview struct {
	ID          uuid.UUID    `json:"id"`
	Rating      int          `json:"rating"`
	Text        string       `json:"text"`
	Reviewer    ReviewerView `json:"reviewer"`
	BasisKind   string       `json:"basis_kind"`
	PublishedAt *time.Time   `json:"published_at"`
	Reply       *ReplyView   `json:"reply"`
}

// PublicView builds a PublicReview.
func PublicView(rv Review) PublicReview {
	return PublicReview{ID: rv.ID, Rating: rv.Rating, Text: rv.Text, Reviewer: reviewerView(rv), BasisKind: rv.BasisKind,
		PublishedAt: rv.PublishedAt, Reply: replyView(rv)}
}

// AdminProfile is the reviewed profile with its organisation.
type AdminProfile struct {
	ProfileRef
	OrganisationID uuid.UUID `json:"organisation_id"`
}

// AdminReviewer is everything the platform knows of the reviewer.
type AdminReviewer struct {
	Kind           string     `json:"kind"`
	Name           string     `json:"name"`
	UserID         *uuid.UUID `json:"user_id"`
	OrganisationID *uuid.UUID `json:"organisation_id"`
	AuthorName     string     `json:"author_name"`
}

// AdminModeration is the last moderation.
type AdminModeration struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	By     PersonRef `json:"by"`
}

// AdminReply is the reply with its author.
type AdminReply struct {
	ReplyView
	By PersonRef `json:"by"`
}

// RemovedReply is a reply a moderator removed.
type RemovedReply struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// ReportView is one report.
type ReportView struct {
	ID             uuid.UUID  `json:"id"`
	Reason         string     `json:"reason"`
	ReporterUserID *uuid.UUID `json:"reporter_user_id"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	Resolution     *string    `json:"resolution"`
}

// ReportOf is the view of a Report.
func ReportOf(rp Report) ReportView {
	v := ReportView{ID: rp.ID, Reason: rp.Reason, ReporterUserID: rp.ReporterUserID, Status: rp.Status, CreatedAt: rp.CreatedAt,
		ResolvedAt: rp.ResolvedAt}
	if rp.Resolution != "" {
		res := rp.Resolution
		v.Resolution = &res
	}
	return v
}

// AdminReview is a review as moderators see it.
type AdminReview struct {
	ID           uuid.UUID        `json:"id"`
	Profile      AdminProfile     `json:"profile"`
	Reviewer     AdminReviewer    `json:"reviewer"`
	Basis        BasisRef         `json:"basis"`
	Rating       int              `json:"rating"`
	Text         string           `json:"text"`
	Status       string           `json:"status"`
	StatusLabel  string           `json:"status_label"`
	Moderation   *AdminModeration `json:"moderation"`
	Reply        *AdminReply      `json:"reply"`
	ReplyRemoved *RemovedReply    `json:"reply_removed"`
	OpenReports  int              `json:"open_reports"`
	// Reports is in the detail only.
	Reports     *[]ReportView `json:"reports,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	PublishedAt *time.Time    `json:"published_at"`
	Can         struct {
		Publish     bool `json:"publish"`
		Hide        bool `json:"hide"`
		Reject      bool `json:"reject"`
		RemoveReply bool `json:"remove_reply"`
	} `json:"can"`
}

// AdminView builds an AdminReview (reports nil: the list's).
func AdminView(rv Review, reports []Report) AdminReview {
	v := AdminReview{ID: rv.ID, Profile: AdminProfile{ProfileRef: reviewProfile(rv), OrganisationID: rv.ProviderOrganisationID},
		Reviewer: AdminReviewer{Kind: rv.ReviewerKind, Name: rv.ReviewerName, UserID: rv.ReviewerUserID,
			OrganisationID: rv.ReviewerOrganisationID, AuthorName: rv.AuthorName},
		Basis:  BasisRef{Kind: rv.BasisKind, ID: rv.BasisID, Reference: rv.BasisReference},
		Rating: rv.Rating, Text: rv.Text, Status: rv.Status, StatusLabel: StatusLabel(rv.Status), OpenReports: rv.OpenReports,
		CreatedAt: rv.CreatedAt, UpdatedAt: rv.UpdatedAt, PublishedAt: rv.PublishedAt}
	if rv.ModeratedAt != nil {
		v.Moderation = &AdminModeration{Reason: rv.ModerationReason, At: *rv.ModeratedAt, By: PersonRef{UserID: rv.ModeratedBy, Name: rv.ModeratedByName}}
	}
	if rv.Reply != nil {
		v.Reply = &AdminReply{ReplyView: *replyView(rv), By: PersonRef{UserID: rv.Reply.ByUserID, Name: rv.Reply.ByName}}
	}
	if rv.ReplyRemovedAt != nil {
		v.ReplyRemoved = &RemovedReply{At: *rv.ReplyRemovedAt, Reason: rv.ReplyRemovedReason}
	}
	if reports != nil {
		list := make([]ReportView, 0, len(reports))
		for _, rp := range reports {
			list = append(list, ReportOf(rp))
		}
		v.Reports = &list
	}
	for _, a := range Allowed(rv) {
		switch a {
		case ActionPublish:
			v.Can.Publish = true
		case ActionHide:
			v.Can.Hide = true
		case ActionReject:
			v.Can.Reject = true
		case ActionRemoveReply:
			v.Can.RemoveReply = true
		}
	}
	return v
}
