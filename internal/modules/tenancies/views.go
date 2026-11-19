package tenancies

import (
	"time"

	"github.com/google/uuid"
)

// The JSON shapes of the API (contract 0.36, "Response shapes (a)").

// PersonRef names a person.
type PersonRef struct {
	UserID *uuid.UUID `json:"user_id"`
	Name   string     `json:"name"`
}

// ContactRef is a tenancy party (a CRM contact).
type ContactRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Live bool      `json:"live"`
}

// ListingRef is a linked listing.
type ListingRef struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Reference string    `json:"reference"`
}

// LeaseRef is a linked lease.
type LeaseRef struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
}

// MoneyView is an amount in a currency.
type MoneyView struct {
	Amount    string `json:"amount"`
	Currency  string `json:"currency"`
	MinorUnit int    `json:"minor_unit"`
}

// RentView is the rent rule.
type RentView struct {
	Amount         string `json:"amount"`
	Currency       string `json:"currency"`
	MinorUnit      int    `json:"minor_unit"`
	Frequency      string `json:"frequency"`
	FrequencyLabel string `json:"frequency_label"`
	DueDay         int    `json:"due_day"`
}

// UnitView is a live unit of a property.
type UnitView struct {
	ID              uuid.UUID  `json:"id"`
	Label           string     `json:"label"`
	Position        int        `json:"position"`
	ActiveTenancyID *uuid.UUID `json:"active_tenancy_id"`
}

// PropertyCan is what the caller may do with a property now.
type PropertyCan struct {
	Edit   bool `json:"edit"`
	Delete bool `json:"delete"`
}

// PropertyView is a property as the API shows it.
type PropertyView struct {
	ID              uuid.UUID   `json:"id"`
	Name            string      `json:"name"`
	Address         string      `json:"address"`
	MarketCode      string      `json:"market_code"`
	Listing         *ListingRef `json:"listing"`
	Notes           string      `json:"notes"`
	Units           []UnitView  `json:"units"`
	UnitCount       int         `json:"unit_count"`
	LettableUnits   int         `json:"lettable_units"`
	OccupiedUnits   int         `json:"occupied_units"`
	ActiveTenancyID *uuid.UUID  `json:"active_tenancy_id"`
	OpenTenancies   int         `json:"open_tenancies"`
	CreatedBy       PersonRef   `json:"created_by"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	Can             PropertyCan `json:"can"`
}

// PropertyPage is one page of properties.
type PropertyPage struct {
	Items    []PropertyView `json:"items"`
	Total    int            `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
}

// TenancyPropertyRef is the property of a tenancy.
type TenancyPropertyRef struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Address    string    `json:"address"`
	MarketCode string    `json:"market_code"`
	Archived   bool      `json:"archived"`
}

// TenancyUnitRef is the unit of a tenancy.
type TenancyUnitRef struct {
	ID       uuid.UUID `json:"id"`
	Label    string    `json:"label"`
	Archived bool      `json:"archived"`
}

// TenancyCan is what the caller may do with a tenancy now.
type TenancyCan struct {
	Edit     bool `json:"edit"`
	Activate bool `json:"activate"`
	End      bool `json:"end"`
	Delete   bool `json:"delete"`
}

// TenancyView is a tenancy as the agent API shows it.
type TenancyView struct {
	ID          uuid.UUID          `json:"id"`
	Status      string             `json:"status"`
	StatusLabel string             `json:"status_label"`
	Property    TenancyPropertyRef `json:"property"`
	Unit        *TenancyUnitRef    `json:"unit"`
	Tenants     []ContactRef       `json:"tenants"`
	Landlords   []ContactRef       `json:"landlords"`
	Lease       *LeaseRef          `json:"lease"`
	Rent        RentView           `json:"rent"`
	Deposit     *MoneyView         `json:"deposit"`
	StartDate   string             `json:"start_date"`
	EndDate     *string            `json:"end_date"`
	EndedOn     *string            `json:"ended_on"`
	EndReason   *string            `json:"end_reason"`
	Notes       string             `json:"notes"`
	ActivatedAt *time.Time         `json:"activated_at"`
	ActivatedBy *PersonRef         `json:"activated_by"`
	EndedAt     *time.Time         `json:"ended_at"`
	EndedBy     *PersonRef         `json:"ended_by"`
	CreatedBy   PersonRef          `json:"created_by"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
	Can         TenancyCan         `json:"can"`
}

// TenancyPage is one page of tenancies.
type TenancyPage struct {
	Items        []TenancyView  `json:"items"`
	Total        int            `json:"total"`
	Page         int            `json:"page"`
	PageSize     int            `json:"page_size"`
	StatusCounts map[string]int `json:"status_counts"`
}

// CodeLabel is one option of a select.
type CodeLabel struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// CurrencyOption is a currency a tenancy may use.
type CurrencyOption struct {
	Code      string `json:"code"`
	MinorUnit int    `json:"minor_unit"`
}

// Permissions are the caller's rights in the book.
type Permissions struct {
	Write bool `json:"write"`
}

// Options is GET .../tenancies/options.
type Options struct {
	Frequencies []CodeLabel      `json:"frequencies"`
	Statuses    []CodeLabel      `json:"statuses"`
	Currencies  []CurrencyOption `json:"currencies"`
	Permissions Permissions      `json:"permissions"`
}

// NameRef is a name only (the tenant view).
type NameRef struct {
	Name string `json:"name"`
}

// LabelRef is a label only (the tenant view).
type LabelRef struct {
	Label string `json:"label"`
}

// ManagerRef says who manages a tenancy (the tenant view).
type ManagerRef struct {
	// Kind is ORGANISATION or DIRECT_OWNER.
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Manager kinds.
const (
	ManagerOrganisation = "ORGANISATION"
	ManagerDirectOwner  = "DIRECT_OWNER"
)

// MyPropertyRef is the property as the tenant sees it.
type MyPropertyRef struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	MarketCode string `json:"market_code"`
}

// MyTenancyView is a tenancy as its tenant sees it: no notes, contact IDs,
// contact details, audit names or rights.
type MyTenancyView struct {
	ID          uuid.UUID     `json:"id"`
	Status      string        `json:"status"`
	StatusLabel string        `json:"status_label"`
	ManagedBy   ManagerRef    `json:"managed_by"`
	Property    MyPropertyRef `json:"property"`
	Unit        *LabelRef     `json:"unit"`
	Tenants     []NameRef     `json:"tenants"`
	Landlords   []NameRef     `json:"landlords"`
	Lease       *LeaseRef     `json:"lease"`
	Rent        RentView      `json:"rent"`
	Deposit     *MoneyView    `json:"deposit"`
	StartDate   string        `json:"start_date"`
	EndDate     *string       `json:"end_date"`
	EndedOn     *string       `json:"ended_on"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
