package notifications

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/mail"
)

func rolesOf(roles []identity.AccountRole) []mail.Role {
	out := make([]mail.Role, 0, len(roles))
	for _, r := range roles {
		out = append(out, mail.Role{Code: r.Role, Name: mail.RoleName(r.Role), Pending: r.Status != identity.AccountRoleActive})
	}
	return out
}

// ---------------------------------------------------------------------------
// Memory source (development and tests)
// ---------------------------------------------------------------------------

// MemorySource reads from the in-memory stores.
type MemorySource struct {
	identity  *identity.MemoryStore
	requests  *propertyrequests.MemoryStore
	enquiries *businessenquiries.MemoryStore
}

// NewMemorySource builds the in-memory Source.
func NewMemorySource(identityStore *identity.MemoryStore, requests *propertyrequests.MemoryStore, enquiries *businessenquiries.MemoryStore) *MemorySource {
	return &MemorySource{identity: identityStore, requests: requests, enquiries: enquiries}
}

func (s *MemorySource) person(ctx context.Context, id uuid.UUID) (Person, bool) {
	u, err := s.identity.UserByID(ctx, id)
	if err != nil || u.Status != identity.StatusActive {
		return Person{}, false
	}
	return Person{UserID: &u.ID, Name: u.Name, Email: u.Email}, true
}

// recipients resolves a routed user or organisation to the people to notify.
func (s *MemorySource) recipients(ctx context.Context, userID, organisationID *uuid.UUID) []Person {
	if userID != nil {
		if p, ok := s.person(ctx, *userID); ok {
			return []Person{p}
		}
		return nil
	}
	if organisationID == nil {
		return nil
	}
	var out []Person
	for _, u := range s.identity.OrganisationManagers(*organisationID) {
		id := u.ID
		out = append(out, Person{UserID: &id, Name: u.Name, Email: u.Email})
	}
	return out
}

// PropertyRequest implements Source.
func (s *MemorySource) PropertyRequest(ctx context.Context, id uuid.UUID) (PropertyRequestInfo, error) {
	for _, req := range s.requests.Requests() {
		if req.ID != id {
			continue
		}
		info := PropertyRequestInfo{
			Reference: req.Reference, ListingID: req.ListingID, RequestType: req.RequestType, Message: req.Message,
			CreatedAt: req.CreatedAt, Customer: Person{UserID: req.CustomerUserID, Name: req.GuestName, Email: req.GuestEmail},
		}
		if req.Recipient != nil {
			info.Recipients = s.recipients(ctx, req.Recipient.UserID, req.Recipient.OrganisationID)
		}
		return info, nil
	}
	return PropertyRequestInfo{}, ErrNotFound
}

// BusinessEnquiry implements Source.
func (s *MemorySource) BusinessEnquiry(ctx context.Context, id uuid.UUID) (BusinessEnquiryInfo, error) {
	for _, enq := range s.enquiries.Enquiries() {
		if enq.ID != id {
			continue
		}
		info := BusinessEnquiryInfo{
			Reference: enq.Reference, ProfileName: enq.ProfileName, Service: enq.Service, Message: enq.Message,
			CreatedAt: enq.CreatedAt, Customer: Person{UserID: enq.CustomerUserID, Name: enq.GuestName, Email: enq.GuestEmail},
		}
		if enq.Recipient != nil {
			org := enq.Recipient.OrganisationID
			info.Recipients = s.recipients(ctx, enq.Recipient.UserID, &org)
		}
		return info, nil
	}
	return BusinessEnquiryInfo{}, ErrNotFound
}

// User implements Source.
func (s *MemorySource) User(ctx context.Context, id uuid.UUID) (UserInfo, error) {
	u, err := s.identity.UserByID(ctx, id)
	if errors.Is(err, identity.ErrNotFound) {
		return UserInfo{}, ErrNotFound
	}
	if err != nil {
		return UserInfo{}, err
	}
	return UserInfo{
		Person: Person{UserID: &u.ID, Name: u.Name, Email: u.Email},
		Roles:  rolesOf(u.AccountRoles), Administrator: len(u.PlatformRoles) > 0,
	}, nil
}

// ---------------------------------------------------------------------------
// Postgres source
// ---------------------------------------------------------------------------

// PostgresSource reads from the database.
//
// NOTE: compiled but not yet exercised against a real database.
type PostgresSource struct{ pool *pgxpool.Pool }

// NewPostgresSource builds the Postgres Source.
func NewPostgresSource(pool *pgxpool.Pool) *PostgresSource { return &PostgresSource{pool: pool} }

func (s *PostgresSource) people(ctx context.Context, sql string, arg any) ([]Person, error) {
	rows, err := s.pool.Query(ctx, sql, arg)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Person, error) {
		var id uuid.UUID
		var p Person
		err := row.Scan(&id, &p.Name, &p.Email)
		p.UserID = &id
		return p, err
	})
}

// recipients resolves a routed user or organisation to the people to notify:
// the named active user, otherwise the organisation's active owners and managers.
func (s *PostgresSource) recipients(ctx context.Context, userID, organisationID *uuid.UUID) ([]Person, error) {
	switch {
	case userID != nil:
		return s.people(ctx, `SELECT id, full_name, email FROM users WHERE id = $1 AND status = 'ACTIVE'`, *userID)
	case organisationID != nil:
		return s.people(ctx, `
			SELECT u.id, u.full_name, u.email
			FROM memberships m
			JOIN users u ON u.id = m.user_id
			WHERE m.organisation_id = $1 AND m.status = 'ACTIVE' AND m.role IN ('OWNER', 'MANAGER') AND u.status = 'ACTIVE'
			ORDER BY u.email`, *organisationID)
	}
	return nil, nil
}

// PropertyRequest implements Source.
func (s *PostgresSource) PropertyRequest(ctx context.Context, id uuid.UUID) (PropertyRequestInfo, error) {
	var info PropertyRequestInfo
	var recipientUser, recipientOrg *uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT pr.reference, pr.listing_id, pr.request_type, pr.message, pr.created_at,
		       pr.customer_user_id, COALESCE(pr.guest_name, cu.full_name, ''), COALESCE(pr.guest_email, cu.email, ''),
		       pr.recipient_user_id, pr.recipient_organisation_id
		FROM property_requests pr
		LEFT JOIN users cu ON cu.id = pr.customer_user_id
		WHERE pr.id = $1`, id).Scan(
		&info.Reference, &info.ListingID, &info.RequestType, &info.Message, &info.CreatedAt,
		&info.Customer.UserID, &info.Customer.Name, &info.Customer.Email, &recipientUser, &recipientOrg)
	if errors.Is(err, pgx.ErrNoRows) {
		return PropertyRequestInfo{}, ErrNotFound
	}
	if err != nil {
		return PropertyRequestInfo{}, fmt.Errorf("load property request: %w", err)
	}
	if info.Recipients, err = s.recipients(ctx, recipientUser, recipientOrg); err != nil {
		return PropertyRequestInfo{}, fmt.Errorf("load request recipients: %w", err)
	}
	return info, nil
}

// BusinessEnquiry implements Source.
func (s *PostgresSource) BusinessEnquiry(ctx context.Context, id uuid.UUID) (BusinessEnquiryInfo, error) {
	var info BusinessEnquiryInfo
	var recipientUser, recipientOrg *uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT e.reference, p.name, COALESCE(e.service, ''), e.message, e.created_at,
		       e.customer_user_id, COALESCE(e.guest_name, cu.full_name, ''), COALESCE(e.guest_email, cu.email, ''),
		       e.recipient_user_id, e.recipient_organisation_id
		FROM business_enquiries e
		JOIN directory_profiles p ON p.id = e.profile_id
		LEFT JOIN users cu ON cu.id = e.customer_user_id
		WHERE e.id = $1`, id).Scan(
		&info.Reference, &info.ProfileName, &info.Service, &info.Message, &info.CreatedAt,
		&info.Customer.UserID, &info.Customer.Name, &info.Customer.Email, &recipientUser, &recipientOrg)
	if errors.Is(err, pgx.ErrNoRows) {
		return BusinessEnquiryInfo{}, ErrNotFound
	}
	if err != nil {
		return BusinessEnquiryInfo{}, fmt.Errorf("load business enquiry: %w", err)
	}
	if info.Recipients, err = s.recipients(ctx, recipientUser, recipientOrg); err != nil {
		return BusinessEnquiryInfo{}, fmt.Errorf("load enquiry recipients: %w", err)
	}
	return info, nil
}

// User implements Source.
func (s *PostgresSource) User(ctx context.Context, id uuid.UUID) (UserInfo, error) {
	var info UserInfo
	var userID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.full_name, u.email,
		       EXISTS (SELECT 1 FROM platform_role_assignments a WHERE a.user_id = u.id)
		FROM users u WHERE u.id = $1`, id).Scan(&userID, &info.Name, &info.Email, &info.Administrator)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserInfo{}, ErrNotFound
	}
	if err != nil {
		return UserInfo{}, fmt.Errorf("load user: %w", err)
	}
	info.UserID = &userID

	rows, err := s.pool.Query(ctx, `SELECT role, status FROM user_account_roles WHERE user_id = $1`, id)
	if err != nil {
		return UserInfo{}, fmt.Errorf("load account roles: %w", err)
	}
	roles, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (identity.AccountRole, error) {
		var r identity.AccountRole
		err := row.Scan(&r.Role, &r.Status)
		return r, err
	})
	if err != nil {
		return UserInfo{}, fmt.Errorf("load account roles: %w", err)
	}
	info.Roles = rolesOf(identity.SortedAccountRoles(roles))
	return info, nil
}
