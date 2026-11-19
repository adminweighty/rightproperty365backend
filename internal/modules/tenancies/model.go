// Package tenancies is property management, part (a) (API 0.36.0, BR-019,
// FR-047): an organisation's (or an approved direct owner's) properties,
// their lettable units and the tenancies on them. Rent schedules and
// receipts (b), maintenance (c) and inspections (d) attach to these rows.
//
// Rules:
//
//   - Every record belongs to a book: an organisation ID or a direct owner's
//     own user ID, the same books as the CRM and the ledger. A book that is
//     not the caller's is 404 BOOK_NOT_FOUND on every route, before any
//     validation (auth.MayReadProperties / MayWriteProperties /
//     OwnsPropertyBook).
//   - A property with no units is one lettable unit. Properties and units
//     are archived, never deleted, so the tenancies (and later maintenance
//     and inspections) that used them keep pointing at them; neither may be
//     archived while a DRAFT or ACTIVE tenancy uses it.
//   - A tenancy's tenants (1+) and landlords are live CRM contacts of the
//     same book; its optional lease is one of the book's leases. Rent and
//     deposit are exact decimals (internal/platform/money) with an ISO 4217
//     currency; the deposit is in the rent's currency.
//   - Status: DRAFT (editable, deletable) -> ACTIVE (one per unit, or per
//     whole property) -> ENDED, or TERMINATED when it ends before its agreed
//     end date. Creating, activating, ending and deleting a tenancy is
//     written to the organisation's audit trail in the same transaction (a
//     direct owner's book: the API log).
//   - A tenant reads their own tenancies (never a DRAFT) when one of its
//     tenant contacts has their verified account email, in the tenancy's
//     own book. Nothing else of the book is visible to them.
package tenancies

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Statuses.
const (
	StatusDraft      = "DRAFT"
	StatusActive     = "ACTIVE"
	StatusEnded      = "ENDED"
	StatusTerminated = "TERMINATED"
)

// Statuses returns every status in lifecycle order.
func Statuses() []string { return []string{StatusDraft, StatusActive, StatusEnded, StatusTerminated} }

var statusLabels = map[string]string{StatusDraft: "Draft", StatusActive: "Active", StatusEnded: "Ended", StatusTerminated: "Terminated early"}

// StatusLabel is the wording of a status.
func StatusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

// Open reports whether a tenancy in that status holds its unit (DRAFT or ACTIVE).
func Open(status string) bool { return status == StatusDraft || status == StatusActive }

// Rent frequencies.
const (
	FrequencyWeekly    = "WEEKLY"
	FrequencyMonthly   = "MONTHLY"
	FrequencyQuarterly = "QUARTERLY"
	FrequencyAnnually  = "ANNUALLY"
)

// Frequencies returns every rent frequency in form order.
func Frequencies() []string {
	return []string{FrequencyWeekly, FrequencyMonthly, FrequencyQuarterly, FrequencyAnnually}
}

var frequencyLabels = map[string]string{FrequencyWeekly: "Weekly", FrequencyMonthly: "Monthly", FrequencyQuarterly: "Quarterly", FrequencyAnnually: "Annually"}

// FrequencyLabel is the wording of a frequency.
func FrequencyLabel(f string) string {
	if l, ok := frequencyLabels[f]; ok {
		return l
	}
	return f
}

// MaxDueDay is the largest due day of a frequency: an ISO weekday (1 =
// Monday) for WEEKLY, a day of the month otherwise.
func MaxDueDay(frequency string) int {
	if frequency == FrequencyWeekly {
		return 7
	}
	return 31
}

// Party roles.
const (
	RoleTenant   = "TENANT"
	RoleLandlord = "LANDLORD"
)

// Limits.
const (
	MaxName      = 200
	MaxAddress   = 500
	MaxNotes     = 5000
	MaxUnitLabel = 80
	MaxUnits     = 200
	MaxTenants   = 10
	MaxLandlords = 10
	MaxReason    = 500
)

// Errors.
var (
	ErrUnauthenticated  = errors.New("not signed in")
	ErrBookNotFound     = errors.New("book not found")
	ErrPropertyNotFound = errors.New("property not found")
	ErrUnitNotFound     = errors.New("unit not found")
	ErrTenancyNotFound  = errors.New("tenancy not found")
	ErrForbidden        = errors.New("not allowed")
	ErrPropertyInUse    = errors.New("property has open tenancies")
	ErrUnitInUse        = errors.New("unit has open tenancies")
	ErrUnitOccupied     = errors.New("unit already has an active tenancy")
	ErrInvalidState     = errors.New("tenancy is not in the right status")
	ErrUnitLabelTaken   = errors.New("unit label taken")
	ErrConflict         = errors.New("changed meanwhile")
)

// Book is whose records: an organisation's, or a direct owner's personal
// ones. Exactly one field is set.
type Book struct {
	OrganisationID *uuid.UUID
	UserID         *uuid.UUID
}

// OrganisationBook returns the book of an organisation.
func OrganisationBook(id uuid.UUID) Book { return Book{OrganisationID: &id} }

// PersonalBook returns the personal book of a direct owner.
func PersonalBook(userID uuid.UUID) Book { return Book{UserID: &userID} }

// ID is the organisation ID or the owner's user ID.
func (b Book) ID() uuid.UUID {
	if b.OrganisationID != nil {
		return *b.OrganisationID
	}
	if b.UserID != nil {
		return *b.UserID
	}
	return uuid.Nil
}

// Same reports whether two books are the same.
func (b Book) Same(o Book) bool {
	return ptrEq(b.OrganisationID, o.OrganisationID) && ptrEq(b.UserID, o.UserID)
}

func ptrEq(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Property is one property of a book. Units holds every unit, archived ones
// included (DeletedAt set), by position.
type Property struct {
	ID            uuid.UUID
	Book          Book
	Name          string
	Address       string
	MarketCode    string
	ListingID     *uuid.UUID
	Notes         string
	CreatedBy     *uuid.UUID
	CreatedByName string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
	Units         []Unit
}

// LiveUnits returns the units that are not archived.
func (p Property) LiveUnits() []Unit {
	out := []Unit{}
	for _, u := range p.Units {
		if u.DeletedAt == nil {
			out = append(out, u)
		}
	}
	return out
}

// Unit returns one unit of the property (archived ones included).
func (p Property) Unit(id uuid.UUID) (Unit, bool) {
	for _, u := range p.Units {
		if u.ID == id {
			return u, true
		}
	}
	return Unit{}, false
}

// Unit is a lettable unit of a property.
type Unit struct {
	ID         uuid.UUID
	PropertyID uuid.UUID
	Label      string
	Position   int
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletedAt  *time.Time
}

// Party is a tenant or landlord of a tenancy: a CRM contact, with the name
// it had when it was set.
type Party struct {
	ContactID uuid.UUID
	Role      string
	Name      string
}

// Tenancy is one tenancy. Amounts are positive decimals with exactly the
// currency's minor-unit decimals; dates are "YYYY-MM-DD" ("" when none).
type Tenancy struct {
	ID              uuid.UUID
	Book            Book
	BookName        string
	PropertyID      uuid.UUID
	UnitID          *uuid.UUID
	LeaseID         *uuid.UUID
	Parties         []Party
	RentAmount      string
	RentCurrency    string
	RentFrequency   string
	RentDueDay      int
	DepositAmount   string
	DepositCurrency string
	StartDate       string
	EndDate         string
	EndedOn         string
	Status          string
	Notes           string
	EndReason       string
	ActivatedAt     *time.Time
	ActivatedBy     *uuid.UUID
	ActivatedByName string
	EndedAt         *time.Time
	EndedBy         *uuid.UUID
	EndedByName     string
	CreatedBy       *uuid.UUID
	CreatedByName   string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// PartyIDs returns the contact IDs of one role, in order.
func (t Tenancy) PartyIDs(role string) []uuid.UUID {
	out := []uuid.UUID{}
	for _, p := range t.Parties {
		if p.Role == role {
			out = append(out, p.ContactID)
		}
	}
	return out
}

// HasTenant reports whether the contact is one of the tenancy's tenants.
func (t Tenancy) HasTenant(contactID uuid.UUID) bool {
	for _, p := range t.Parties {
		if p.Role == RoleTenant && p.ContactID == contactID {
			return true
		}
	}
	return false
}

// SameUnit reports whether the tenancy is on the unit (nil: the whole property).
func (t Tenancy) SameUnit(propertyID uuid.UUID, unitID *uuid.UUID) bool {
	return t.PropertyID == propertyID && ptrEq(t.UnitID, unitID)
}

// Use is an open (DRAFT or ACTIVE) tenancy of a property.
type Use struct {
	TenancyID  uuid.UUID
	PropertyID uuid.UUID
	UnitID     *uuid.UUID
	Status     string
}

// PropertyFilter narrows a list of properties (live ones only).
type PropertyFilter struct {
	Q          string
	MarketCode string
	// Limit 0 means no limit.
	Limit, Offset int
}

// TenancyFilter narrows a list of tenancies.
type TenancyFilter struct {
	Status     string
	PropertyID *uuid.UUID
	UnitID     *uuid.UUID
	ContactID  *uuid.UUID
	// Limit 0 means no limit.
	Limit, Offset int
}

// Change is one atomic write. Stores apply it in this order: lock the
// tenancies of ExpectStatus and fail with ErrConflict when one is missing
// or has another status; lock the properties of PropertyFree and fail with
// ErrPropertyInUse when one has an open tenancy, and the same for UnitFree
// (ErrUnitInUse); upsert Properties (by ID; the units are not touched);
// upsert Units (a live label taken in the property: ErrUnitLabelTaken);
// delete DeleteTenancies; upsert Tenancies with their parties (a second
// ACTIVE tenancy of a unit: ErrUnitOccupied; a property that is archived:
// ErrPropertyNotFound); insert Audit.
type Change struct {
	ExpectStatus    map[uuid.UUID]string
	PropertyFree    []uuid.UUID
	UnitFree        []uuid.UUID
	Properties      []Property
	Units           []Unit
	DeleteTenancies []uuid.UUID
	Tenancies       []Tenancy
	Audit           []orgaudit.Event
}

// Store persists properties, units and tenancies.
type Store interface {
	// Property returns a property with all its units (archived ones too), or ErrPropertyNotFound.
	Property(ctx context.Context, id uuid.UUID) (Property, error)
	// Properties returns one page of a book's live properties, newest first, and the total.
	Properties(ctx context.Context, book Book, f PropertyFilter) ([]Property, int, error)
	// PropertiesByIDs returns the properties with those IDs (archived ones too).
	PropertiesByIDs(ctx context.Context, ids []uuid.UUID) ([]Property, error)
	// Uses returns the open (DRAFT or ACTIVE) tenancies of the properties.
	Uses(ctx context.Context, propertyIDs []uuid.UUID) ([]Use, error)
	// Tenancy returns one tenancy with its parties, or ErrTenancyNotFound.
	Tenancy(ctx context.Context, id uuid.UUID) (Tenancy, error)
	// Tenancies returns one page of a book's tenancies, newest first, the
	// total, and the counts per status (the filter's Status ignored).
	Tenancies(ctx context.Context, book Book, f TenancyFilter) ([]Tenancy, int, map[string]int, error)
	// TenanciesOfTenants returns the tenancies that are not DRAFT and have
	// one of the contacts as a tenant.
	TenanciesOfTenants(ctx context.Context, contactIDs []uuid.UUID) ([]Tenancy, error)
	// Apply writes a Change atomically.
	Apply(ctx context.Context, ch Change) error
}

func newID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}
