-- API 0.57.0: branches (FR-002).
CREATE TABLE organisation_branches (
    id              uuid PRIMARY KEY,
    organisation_id uuid        NOT NULL REFERENCES organisations (id) ON DELETE CASCADE,
    name            text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    active          boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);
CREATE UNIQUE INDEX organisation_branches_name_key ON organisation_branches (organisation_id, lower(name));

-- A member pinned to a branch; a listing tied to one; a ledger entry's branch.
ALTER TABLE memberships ADD COLUMN branch_id uuid REFERENCES organisation_branches (id) ON DELETE SET NULL;
ALTER TABLE listings ADD COLUMN branch_id uuid REFERENCES organisation_branches (id) ON DELETE SET NULL;
ALTER TABLE ledger_entries ADD COLUMN branch_id uuid REFERENCES organisation_branches (id) ON DELETE SET NULL;
CREATE INDEX listings_branch_idx ON listings (branch_id) WHERE branch_id IS NOT NULL;

-- The free-text branches of organisation ledgers become branches (the first
-- spelling recorded wins); entries keep their text (they are immutable) and
-- gain the branch_id, which the immutability guard does not cover.
INSERT INTO organisation_branches (id, organisation_id, name, active, created_at, updated_at)
SELECT gen_random_uuid(), x.organisation_id, x.branch, true, now(), now()
FROM (SELECT DISTINCT ON (organisation_id, lower(trim(branch))) organisation_id, trim(branch) AS branch
      FROM ledger_entries
      WHERE organisation_id IS NOT NULL AND branch IS NOT NULL AND char_length(trim(branch)) BETWEEN 1 AND 80
      ORDER BY organisation_id, lower(trim(branch)), recorded_at) x;
UPDATE ledger_entries e SET branch_id = b.id
FROM organisation_branches b
WHERE e.organisation_id = b.organisation_id AND e.branch IS NOT NULL AND lower(trim(e.branch)) = lower(b.name);

-- Branches are audited in the organisation audit log.
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_action_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
    'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
    'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
    'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
    'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED',
    'CONTACT_OWNER_CHANGED', 'CONTACTS_MERGED', 'CONTACTS_IMPORTED', 'CONTACTS_EXPORTED',
    'APPLICATION_VIEWED', 'APPLICATION_FILE_VIEWED',
    'VENUE_BOOKING_CONFIRMED', 'VENUE_BOOKING_DECLINED', 'VENUE_BOOKING_CANCELLED',
    'VENUE_BOOKING_TIME_PROPOSED', 'VENUE_BOOKING_COMPLETED', 'VENUE_BOOKING_NO_SHOW',
    'LEDGER_ENTRY_CREATED', 'LEDGER_ENTRY_REVERSED', 'LEDGER_ENTRY_CORRECTED', 'LEDGER_ENTRY_RECONCILED',
    'LEDGER_ENTRY_UNRECONCILED', 'LEDGER_ATTACHMENT_ADDED', 'LEDGER_ATTACHMENT_VIEWED', 'LEDGER_EXPORTED',
    'TENANCY_CREATED', 'TENANCY_ACTIVATED', 'TENANCY_ENDED', 'TENANCY_DELETED',
    'RENT_RECEIPT_RECORDED', 'RENT_DUE_WAIVED',
    'MAINTENANCE_ASSIGNED', 'MAINTENANCE_COST_RECORDED', 'MAINTENANCE_CANCELLED',
    'INSPECTION_COMPLETED', 'INSPECTION_CANCELLED',
    'BRANCH_CREATED', 'BRANCH_UPDATED', 'MEMBER_BRANCH_CHANGED', 'LISTING_BRANCH_CHANGED'));
ALTER TABLE organisation_audit_events DROP CONSTRAINT IF EXISTS organisation_audit_events_target_type_check;
ALTER TABLE organisation_audit_events ADD CONSTRAINT organisation_audit_events_target_type_check
    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST', 'CONTACT', 'CONTACT_BOOK', 'APPLICATION',
                           'VENUE_BOOKING', 'LEDGER_ENTRY', 'LEDGER_BOOK', 'TENANCY', 'MAINTENANCE_REQUEST', 'INSPECTION',
                           'BRANCH', 'LISTING'));
