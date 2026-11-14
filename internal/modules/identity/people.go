package identity

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file reads people in bulk: the members of an organisation and the
// account numbers of the administrators' overview. Neither is ever used to
// decide what a caller may do.

// Member statuses. A membership that was removed is not listed at all.
const (
	MemberActive    = "ACTIVE"
	MemberInvited   = "INVITED"
	MemberSuspended = "SUSPENDED" // the member's account is not active
)

// MemberStatusLabel is the wording shown for a member status.
func MemberStatusLabel(status string) string {
	switch status {
	case MemberActive:
		return "Active"
	case MemberInvited:
		return "Invited"
	case MemberSuspended:
		return "Suspended"
	}
	return status
}

// Member is one person in an organisation.
type Member struct {
	UserID     uuid.UUID
	Name       string
	Role       string // OWNER, MANAGER, AGENT or STAFF
	MarketCode string
	Status     string
	JoinedAt   *time.Time // nil when not recorded (memory store)
}

// RoleCount is the number of accounts that chose a role, by its status.
type RoleCount struct {
	Role   string
	Status string
	Count  int
}

// RecentAccount is one newly registered account.
type RecentAccount struct {
	ID            uuid.UUID
	Name          string
	Email         string
	EmailVerified bool
	Roles         []string
	CreatedAt     time.Time
}

// AccountStats are the account numbers of the administrators' overview.
// Administrator accounts (holders of a platform role) are counted in
// Administrators only.
type AccountStats struct {
	Total          int
	Administrators int
	ByStatus       map[string]int
	Unverified     int // email address not verified yet
	ByRole         []RoleCount
	Last7Days      int
	Last30Days     int
	Recent         []RecentAccount
}

// PeopleStore is the part of Store behind this file.
type PeopleStore interface {
	// OrganisationMembers returns the members of an organisation whose
	// membership was not removed, ordered by role (OWNER first) then name.
	OrganisationMembers(ctx context.Context, organisationID uuid.UUID) ([]Member, error)
	// AccountStats counts accounts and returns the newest `recent` of them.
	AccountStats(ctx context.Context, now time.Time, recent int) (AccountStats, error)
}

var memberRoleRank = map[string]int{"OWNER": 0, "MANAGER": 1, "AGENT": 2, "STAFF": 3}

func sortMembers(members []Member) {
	sort.SliceStable(members, func(i, j int) bool {
		if ri, rj := memberRoleRank[members[i].Role], memberRoleRank[members[j].Role]; ri != rj {
			return ri < rj
		}
		return members[i].Name < members[j].Name
	})
}

func sortRoleCounts(counts []RoleCount) {
	rank := func(role string) int {
		for i, r := range selectableAccountRoles {
			if r == role {
				return i
			}
		}
		return len(selectableAccountRoles)
	}
	sort.SliceStable(counts, func(i, j int) bool {
		if ri, rj := rank(counts[i].Role), rank(counts[j].Role); ri != rj {
			return ri < rj
		}
		return counts[i].Status < counts[j].Status
	})
}

// OrganisationMembers implements Store.
func (s *MemoryStore) OrganisationMembers(_ context.Context, organisationID uuid.UUID) ([]Member, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Member{}
	for _, m := range s.memberships {
		u, ok := s.users[m.UserID]
		if m.OrganisationID != organisationID || !ok {
			continue
		}
		member := Member{UserID: u.ID, Name: u.Name, Role: m.Role, MarketCode: m.MarketCode, Status: MemberActive}
		if u.Status != StatusActive {
			member.Status = MemberSuspended
		}
		out = append(out, member)
	}
	sortMembers(out)
	return out, nil
}

// AccountStats implements Store.
func (s *MemoryStore) AccountStats(_ context.Context, now time.Time, recent int) (AccountStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := AccountStats{ByStatus: map[string]int{}, Recent: []RecentAccount{}}
	byRole := map[RoleCount]int{}
	for id, u := range s.users {
		if len(s.platformRoles[id]) > 0 {
			stats.Administrators++
			continue
		}
		stats.Total++
		stats.ByStatus[u.Status]++
		if u.EmailVerifiedAt == nil {
			stats.Unverified++
		}
		if u.CreatedAt.After(now.AddDate(0, 0, -7)) {
			stats.Last7Days++
		}
		if u.CreatedAt.After(now.AddDate(0, 0, -30)) {
			stats.Last30Days++
		}
		account := RecentAccount{ID: u.ID, Name: u.Name, Email: u.Email, EmailVerified: u.EmailVerifiedAt != nil, Roles: []string{}, CreatedAt: u.CreatedAt}
		roles := append([]AccountRole(nil), s.accountRoles[id]...)
		sortAccountRoles(roles)
		for _, r := range roles {
			byRole[RoleCount{Role: r.Role, Status: r.Status}]++
			account.Roles = append(account.Roles, r.Role)
		}
		stats.Recent = append(stats.Recent, account)
	}
	for k, n := range byRole {
		stats.ByRole = append(stats.ByRole, RoleCount{Role: k.Role, Status: k.Status, Count: n})
	}
	sortRoleCounts(stats.ByRole)
	sort.SliceStable(stats.Recent, func(i, j int) bool {
		if !stats.Recent[i].CreatedAt.Equal(stats.Recent[j].CreatedAt) {
			return stats.Recent[i].CreatedAt.After(stats.Recent[j].CreatedAt)
		}
		return stats.Recent[i].ID.String() > stats.Recent[j].ID.String()
	})
	if len(stats.Recent) > recent {
		stats.Recent = stats.Recent[:max(recent, 0)]
	}
	return stats, nil
}

// OrganisationMembers implements Store.
//
// NOTE: compiled but not yet exercised against a real database.
func (s *PostgresStore) OrganisationMembers(ctx context.Context, organisationID uuid.UUID) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.full_name, m.role, mk.code,
		       CASE WHEN u.status <> 'ACTIVE' THEN 'SUSPENDED' ELSE m.status END, m.created_at
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		JOIN markets mk ON mk.id = m.market_id
		WHERE m.organisation_id = $1 AND m.status IN ('ACTIVE', 'INVITED') AND u.status <> 'DELETED'`, organisationID)
	if err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) {
		var m Member
		var joined time.Time
		err := row.Scan(&m.UserID, &m.Name, &m.Role, &m.MarketCode, &m.Status, &joined)
		joined = joined.UTC()
		m.JoinedAt = &joined
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("load members: %w", err)
	}
	if out == nil {
		out = []Member{}
	}
	sortMembers(out)
	return out, nil
}

// notAdmin excludes holders of a platform role.
const notAdmin = `NOT EXISTS (SELECT 1 FROM platform_role_assignments a WHERE a.user_id = u.id)`

// AccountStats implements Store.
//
// NOTE: compiled but not yet exercised against a real database.
func (s *PostgresStore) AccountStats(ctx context.Context, now time.Time, recent int) (AccountStats, error) {
	stats := AccountStats{ByStatus: map[string]int{}, Recent: []RecentAccount{}}
	if err := s.pool.QueryRow(ctx, `SELECT count(DISTINCT user_id) FROM platform_role_assignments`).Scan(&stats.Administrators); err != nil {
		return AccountStats{}, fmt.Errorf("count administrators: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT u.status, count(*), count(*) FILTER (WHERE u.email_verified_at IS NULL),
		       count(*) FILTER (WHERE u.created_at > $1), count(*) FILTER (WHERE u.created_at > $2)
		FROM users u WHERE `+notAdmin+` GROUP BY u.status`, now.AddDate(0, 0, -7), now.AddDate(0, 0, -30))
	if err != nil {
		return AccountStats{}, fmt.Errorf("count accounts: %w", err)
	}
	for rows.Next() {
		var status string
		var n, unverified, last7, last30 int
		if err := rows.Scan(&status, &n, &unverified, &last7, &last30); err != nil {
			rows.Close()
			return AccountStats{}, fmt.Errorf("count accounts: %w", err)
		}
		stats.ByStatus[status] = n
		stats.Total, stats.Unverified = stats.Total+n, stats.Unverified+unverified
		stats.Last7Days, stats.Last30Days = stats.Last7Days+last7, stats.Last30Days+last30
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return AccountStats{}, fmt.Errorf("count accounts: %w", err)
	}

	rows, err = s.pool.Query(ctx, `
		SELECT r.role, r.status, count(*) FROM user_account_roles r JOIN users u ON u.id = r.user_id
		WHERE `+notAdmin+` GROUP BY r.role, r.status`)
	if err != nil {
		return AccountStats{}, fmt.Errorf("count account roles: %w", err)
	}
	if stats.ByRole, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RoleCount, error) {
		var c RoleCount
		err := row.Scan(&c.Role, &c.Status, &c.Count)
		return c, err
	}); err != nil {
		return AccountStats{}, fmt.Errorf("count account roles: %w", err)
	}
	sortRoleCounts(stats.ByRole)

	rows, err = s.pool.Query(ctx, `
		SELECT u.id, u.full_name, u.email, u.email_verified_at IS NOT NULL, u.created_at,
		       COALESCE((SELECT array_agg(r.role ORDER BY r.role) FROM user_account_roles r WHERE r.user_id = u.id), '{}')
		FROM users u WHERE `+notAdmin+`
		ORDER BY u.created_at DESC, u.id DESC LIMIT $1`, max(recent, 0))
	if err != nil {
		return AccountStats{}, fmt.Errorf("load recent accounts: %w", err)
	}
	recentAccounts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RecentAccount, error) {
		var a RecentAccount
		err := row.Scan(&a.ID, &a.Name, &a.Email, &a.EmailVerified, &a.CreatedAt, &a.Roles)
		a.CreatedAt = a.CreatedAt.UTC()
		return a, err
	})
	if err != nil {
		return AccountStats{}, fmt.Errorf("load recent accounts: %w", err)
	}
	if recentAccounts != nil {
		stats.Recent = recentAccounts
	}
	return stats, nil
}
