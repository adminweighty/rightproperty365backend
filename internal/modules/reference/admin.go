package reference

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
)

// Administration of the reference data (/v1/admin/reference/*).
//
// Every set can be relabelled, reordered, deactivated and reactivated. New
// rows can be added only to the sets whose values are data rather than
// behaviour (CreatableSets): a new account role, request type or deal type
// would mean nothing to the code that branches on them. Nothing is ever
// deleted: deactivating a row hides it from every drop-down and refuses it
// in new input at once, while the listings, profiles and requests that
// already use it keep their code (and a row in use is therefore never lost).

// AdminRoles may change the reference data: SUPER_ADMIN, PLATFORM_ADMIN,
// CONTENT_ADMIN, and MARKET_ADMIN for the rows of its own market only.
func AdminRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformContentAdmin, auth.PlatformMarketAdmin}
}

// CreatableSets are the sets new rows may be added to.
func CreatableSets() []string {
	return []string{SetPropertyTypes, SetListingFeatures, SetServiceTrades, SetRegions, SetCities, SetSuburbs}
}

// marketSets are the sets whose rows belong to one market.
var marketSets = []string{SetRegions, SetCities, SetSuburbs}

// parentSets says which set a row's parent_code points into.
var parentSets = map[string]string{SetPropertyTypes: SetPropertyCategories, SetCities: SetRegions, SetSuburbs: SetCities}

// Errors of the administration.
var (
	ErrDuplicate       = errors.New("a row with this code already exists in the set")
	ErrNotCreatable    = errors.New("rows cannot be added to this set")
	ErrMarketForbidden = errors.New("the row is outside the administrator's market")
)

// Error codes.
const (
	CodeReferenceNotFound        = "REFERENCE_ITEM_NOT_FOUND"
	CodeReferenceDuplicate       = "REFERENCE_ITEM_EXISTS"
	CodeReferenceNotCreatable    = "REFERENCE_SET_NOT_EXTENSIBLE"
	CodeReferenceMarketForbidden = "REFERENCE_MARKET_FORBIDDEN"
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// AdminStore is the write side of the reference data.
type AdminStore interface {
	// Item returns one row in any status, or ErrNotFound.
	Item(ctx context.Context, set, code string) (Item, error)
	// CreateItem adds an active row with its audit record (ErrDuplicate).
	CreateItem(ctx context.Context, set string, it Item, audit adminaudit.Event) error
	// ChangeItem locks the row, calls change with a copy and saves the
	// label, singular label, description, sort order, parent and active
	// flag with the audit record.
	ChangeItem(ctx context.Context, set, code string, change func(it *Item) (adminaudit.Event, error)) (Item, error)
}

var codeRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ItemInput is the body of POST and PATCH /v1/admin/reference/{set}.
type ItemInput struct {
	Code          *string `json:"code"`
	Label         *string `json:"label"`
	SingularLabel *string `json:"singular_label"`
	Description   *string `json:"description"`
	SortOrder     *int    `json:"sort_order"`
	MarketCode    *string `json:"market_code"`
	ParentCode    *string `json:"parent_code"`
}

// Admin is the reference data administration.
type Admin struct {
	svc   *Service
	store AdminStore
	now   func() time.Time
}

// NewAdmin builds the administration over the read service and the write store.
func NewAdmin(svc *Service, store AdminStore, now func() time.Time) *Admin {
	if now == nil {
		now = time.Now
	}
	return &Admin{svc: svc, store: store, now: now}
}

// mayEdit applies the MARKET_ADMIN rule: a market administrator changes
// only the rows of its own market(s).
func mayEdit(p *auth.Principal, market *string) bool {
	if p.HasPlatformRole(auth.PlatformAdmin, auth.PlatformContentAdmin) {
		return true
	}
	if market == nil {
		return false
	}
	for _, r := range p.PlatformRoles {
		if r.Role == auth.PlatformMarketAdmin && r.MarketCode == *market {
			return true
		}
	}
	return false
}

// List returns every row of a set, inactive ones included.
func (a *Admin) List(ctx context.Context, set, market string) ([]Item, error) {
	if !IsSet(set) {
		return nil, ErrUnknownSet
	}
	if err := a.svc.checkMarket(ctx, strings.ToUpper(market)); err != nil {
		return nil, err
	}
	items, err := a.svc.store.List(ctx, set, Filter{Market: strings.ToUpper(market), IncludeInactive: true})
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []Item{}
	}
	return items, nil
}

func textField(fields map[string]string, name string, in *string, max int, required bool) *string {
	if in == nil {
		if required {
			fields[name] = "is required"
		}
		return nil
	}
	s := strings.TrimSpace(*in)
	if s == "" && required {
		fields[name] = "is required"
		return nil
	}
	if utf8.RuneCountInString(s) > max {
		fields[name] = fmt.Sprintf("must be at most %d characters", max)
	}
	if s == "" {
		return nil
	}
	return &s
}

func itemState(it Item) map[string]any {
	m := map[string]any{"label": it.Label, "sort_order": it.SortOrder, "active": it.Active}
	if it.ParentCode != nil {
		m["parent_code"] = *it.ParentCode
	}
	if it.MarketCode != nil {
		m["market_code"] = *it.MarketCode
	}
	return m
}

func targetID(set, code string) string { return set + "/" + code }

// checkParent verifies that a parent code names an active row of the parent set.
func (a *Admin) checkParent(ctx context.Context, set string, parent *string, fields map[string]string) error {
	parentSet, ok := parentSets[set]
	if parent == nil {
		if ok && set != SetPropertyTypes {
			fields["parent_code"] = "is required (a code from " + parentSet + ")"
		}
		return nil
	}
	if !ok {
		fields["parent_code"] = "must be empty for this set"
		return nil
	}
	codes, err := a.svc.ActiveCodes(ctx, parentSet)
	if err != nil {
		return err
	}
	if !slices.Contains(codes, *parent) {
		fields["parent_code"] = "must be an active code from " + parentSet
	}
	return nil
}

// Create adds a row to one of the CreatableSets.
func (a *Admin) Create(ctx context.Context, p *auth.Principal, set string, in ItemInput) (Item, error) {
	if !IsSet(set) {
		return Item{}, ErrUnknownSet
	}
	if !slices.Contains(CreatableSets(), set) {
		return Item{}, ErrNotCreatable
	}
	fields := map[string]string{}
	it := Item{Active: true}
	if code := textField(fields, "code", in.Code, 80, true); code != nil {
		it.Code = *code
		if !codeRe.MatchString(it.Code) {
			fields["code"] = "must be lower-case letters and digits separated by single hyphens (for example \"garden-flat\")"
		}
	}
	if label := textField(fields, "label", in.Label, 120, true); label != nil {
		it.Label = *label
	}
	it.Description = textField(fields, "description", in.Description, 500, false)
	if in.SortOrder != nil {
		it.SortOrder = *in.SortOrder
	}
	if in.MarketCode != nil && strings.TrimSpace(*in.MarketCode) != "" {
		m := strings.ToUpper(strings.TrimSpace(*in.MarketCode))
		if err := a.svc.checkMarket(ctx, m); err != nil {
			fields["market_code"] = "must be a known country code"
		}
		it.MarketCode = &m
	} else if slices.Contains(marketSets, set) {
		fields["market_code"] = "is required for " + set
	}
	it.ParentCode = textField(fields, "parent_code", in.ParentCode, 80, false)
	if err := a.checkParent(ctx, set, it.ParentCode, fields); err != nil {
		return Item{}, err
	}
	if len(fields) > 0 {
		return Item{}, &ValidationError{Fields: fields}
	}
	if !mayEdit(p, it.MarketCode) {
		return Item{}, ErrMarketForbidden
	}
	if _, err := a.store.Item(ctx, set, it.Code); err == nil {
		return Item{}, ErrDuplicate
	} else if !errors.Is(err, ErrNotFound) {
		return Item{}, err
	}
	now := a.now().UTC()
	audit := adminaudit.New(p.UserID, p.Name, adminaudit.ActionReferenceCreated, adminaudit.TargetReferenceItem, targetID(set, it.Code), it.Label, "",
		nil, itemState(it), now)
	if err := a.store.CreateItem(ctx, set, it, audit); err != nil {
		return Item{}, err
	}
	return a.store.Item(ctx, set, it.Code)
}

// Update relabels or reorders a row (and changes its description or parent).
func (a *Admin) Update(ctx context.Context, p *auth.Principal, set, code string, in ItemInput) (Item, error) {
	if !IsSet(set) {
		return Item{}, ErrUnknownSet
	}
	if in.Code != nil || in.MarketCode != nil {
		fields := map[string]string{}
		if in.Code != nil {
			fields["code"] = "cannot be changed (create a new row and deactivate this one)"
		}
		if in.MarketCode != nil {
			fields["market_code"] = "cannot be changed"
		}
		return Item{}, &ValidationError{Fields: fields}
	}
	fields := map[string]string{}
	label := textField(fields, "label", in.Label, 120, false)
	singular := textField(fields, "singular_label", in.SingularLabel, 120, false)
	description := textField(fields, "description", in.Description, 500, false)
	parent := textField(fields, "parent_code", in.ParentCode, 80, false)
	if in.ParentCode != nil {
		if err := a.checkParent(ctx, set, parent, fields); err != nil {
			return Item{}, err
		}
	}
	if len(fields) > 0 {
		return Item{}, &ValidationError{Fields: fields}
	}
	now := a.now().UTC()
	return a.store.ChangeItem(ctx, set, code, func(it *Item) (adminaudit.Event, error) {
		if !mayEdit(p, it.MarketCode) {
			return adminaudit.Event{}, ErrMarketForbidden
		}
		before := itemState(*it)
		if label != nil {
			it.Label = *label
		}
		if in.SingularLabel != nil {
			it.SingularLabel = singular
		}
		if in.Description != nil {
			it.Description = description
		}
		if in.SortOrder != nil {
			it.SortOrder = *in.SortOrder
		}
		if in.ParentCode != nil {
			it.ParentCode = parent
		}
		return adminaudit.New(p.UserID, p.Name, adminaudit.ActionReferenceUpdated, adminaudit.TargetReferenceItem, targetID(set, code), it.Label, "",
			before, itemState(*it), now), nil
	})
}

// SetActive deactivates or reactivates a row. Deactivating never deletes:
// rows that use the code keep it.
func (a *Admin) SetActive(ctx context.Context, p *auth.Principal, set, code string, active bool) (Item, error) {
	if !IsSet(set) {
		return Item{}, ErrUnknownSet
	}
	now := a.now().UTC()
	return a.store.ChangeItem(ctx, set, code, func(it *Item) (adminaudit.Event, error) {
		if !mayEdit(p, it.MarketCode) {
			return adminaudit.Event{}, ErrMarketForbidden
		}
		before := itemState(*it)
		it.Active = active
		action := adminaudit.ActionReferenceDeactivated
		if active {
			action = adminaudit.ActionReferenceReactivated
		}
		return adminaudit.New(p.UserID, p.Name, action, adminaudit.TargetReferenceItem, targetID(set, code), it.Label, "", before, itemState(*it), now), nil
	})
}

// ---------------------------------------------------------------------------
// Stores
// ---------------------------------------------------------------------------

// SetAuditLog makes the memory store write the administrators' audit
// records to log (development and tests).
func (s *MemoryStore) SetAuditLog(log *adminaudit.MemoryLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = log
}

func cloneItem(it Item) Item {
	for _, p := range []**string{&it.SingularLabel, &it.Description, &it.MarketCode, &it.ParentCode} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	if it.Group != nil {
		g := *it.Group
		it.Group = &g
	}
	if it.RequiresVerification != nil {
		v := *it.RequiresVerification
		it.RequiresVerification = &v
	}
	return it
}

// Item implements AdminStore.
func (s *MemoryStore) Item(_ context.Context, set, code string) (Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, it := range s.sets[set] {
		if it.Code == code {
			return cloneItem(it), nil
		}
	}
	return Item{}, ErrNotFound
}

// CreateItem implements AdminStore.
func (s *MemoryStore) CreateItem(_ context.Context, set string, it Item, audit adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.sets[set] {
		if have.Code == it.Code {
			return ErrDuplicate
		}
	}
	s.sets[set] = append(s.sets[set], cloneItem(it))
	if s.audit != nil {
		s.audit.Append(audit)
	}
	return nil
}

// ChangeItem implements AdminStore.
func (s *MemoryStore) ChangeItem(_ context.Context, set, code string, change func(it *Item) (adminaudit.Event, error)) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, have := range s.sets[set] {
		if have.Code != code {
			continue
		}
		next := cloneItem(have)
		audit, err := change(&next)
		if err != nil {
			return Item{}, err
		}
		s.sets[set][i] = cloneItem(next)
		if s.audit != nil {
			s.audit.Append(audit)
		}
		return cloneItem(next), nil
	}
	return Item{}, ErrNotFound
}

const itemColumns = `r.code, r.label, r.singular_label, r.description, r.sort_order, m.code, r.parent_code,
	r.group_code, r.group_label, r.group_sort_order, r.requires_verification, r.active`

func scanItem(row pgx.Row) (Item, error) {
	var it Item
	var groupCode, groupLabel *string
	var groupSort *int
	if err := row.Scan(&it.Code, &it.Label, &it.SingularLabel, &it.Description, &it.SortOrder, &it.MarketCode, &it.ParentCode,
		&groupCode, &groupLabel, &groupSort, &it.RequiresVerification, &it.Active); err != nil {
		return Item{}, err
	}
	if groupCode != nil && groupLabel != nil {
		it.Group = &Group{Code: *groupCode, Label: *groupLabel}
		if groupSort != nil {
			it.Group.SortOrder = *groupSort
		}
	}
	return it, nil
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func pgItem(ctx context.Context, q rowQuerier, set, code string, lock bool) (Item, error) {
	sql := `SELECT ` + itemColumns + ` FROM reference_items r LEFT JOIN markets m ON m.id = r.market_id WHERE r.set_code = $1 AND r.code = $2`
	if lock {
		sql += ` FOR UPDATE OF r`
	}
	it, err := scanItem(q.QueryRow(ctx, sql, set, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("load reference item: %w", err)
	}
	return it, nil
}

// Item implements AdminStore.
func (s *PostgresStore) Item(ctx context.Context, set, code string) (Item, error) {
	return pgItem(ctx, s.pool, set, code, false)
}

// CreateItem implements AdminStore.
func (s *PostgresStore) CreateItem(ctx context.Context, set string, it Item, audit adminaudit.Event) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, active, market_id, parent_code, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, true, (SELECT id FROM markets WHERE code = $7), $8, $9, $9)`,
		set, it.Code, it.Label, it.SingularLabel, it.Description, it.SortOrder, it.MarketCode, it.ParentCode, audit.At); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		return fmt.Errorf("insert reference item: %w", err)
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ChangeItem implements AdminStore.
func (s *PostgresStore) ChangeItem(ctx context.Context, set, code string, change func(it *Item) (adminaudit.Event, error)) (Item, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Item{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	cur, err := pgItem(ctx, tx, set, code, true)
	if err != nil {
		return Item{}, err
	}
	next := cloneItem(cur)
	audit, err := change(&next)
	if err != nil {
		return Item{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE reference_items SET label = $3, singular_label = $4, description = $5, sort_order = $6, parent_code = $7, active = $8, updated_at = $9
		WHERE set_code = $1 AND code = $2`,
		set, code, next.Label, next.SingularLabel, next.Description, next.SortOrder, next.ParentCode, next.Active, audit.At); err != nil {
		return Item{}, fmt.Errorf("update reference item: %w", err)
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return Item{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Item{}, err
	}
	return next, nil
}
