-- 0021_agency_teams: an organisation's OWNER and MANAGER run their own team.
--
--   * memberships.status gains SUSPENDED: a suspended member keeps the row
--     (and can be reactivated) but has no access at all, exactly like a
--     removed one. A removed member is REMOVED (the value 0001 already had)
--     and is no longer listed. Neither is ever read from a cache: every
--     request loads the caller's ACTIVE memberships (identity's loadUser).
--   * memberships.job_title: the title shown in the team list ("Senior
--     negotiator"), optional, at most 80 characters.
--   * organisation_invitations: an invitation to join with a role. Only the
--     SHA-256 of its single-use token is stored; it expires after 7 days.
--     At most one PENDING invitation per organisation and email address:
--     inviting the same address again replaces the old one (REPLACED).
--   * organisation_audit_events: the team's durable audit trail (invites,
--     acceptances, join decisions, role and status changes, removals,
--     assignments), written in the same transaction as each change.
--     Rows are immutable apart from losing the actor to an account deletion.
--   * appointment_timeline.event gains UNASSIGNED: a removed or suspended
--     member's open appointments go back to the organisation.
--
-- Safe on a database with data: no existing row is rewritten, every new
-- object is created only if missing, and the two replaced CHECK constraints
-- only widen what they allow.

-- ---------------------------------------------------------------------------
-- Memberships
-- ---------------------------------------------------------------------------
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_status_check;
ALTER TABLE memberships
    ADD CONSTRAINT memberships_status_check CHECK (status IN ('ACTIVE', 'INVITED', 'SUSPENDED', 'REMOVED'));

ALTER TABLE memberships
    ADD COLUMN IF NOT EXISTS job_title text
        CONSTRAINT memberships_job_title_length CHECK (job_title IS NULL OR char_length(job_title) BETWEEN 1 AND 80);


-- ---------------------------------------------------------------------------
-- Invitations
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS organisation_invitations (
    id              uuid PRIMARY KEY,
    organisation_id uuid NOT NULL REFERENCES organisations (id) ON DELETE CASCADE,
    -- Lower-cased, as users.email is compared.
    email           text NOT NULL CONSTRAINT organisation_invitations_email_check
                    CHECK (email = lower(email) AND char_length(email) BETWEEN 3 AND 254),
    -- Nobody is invited as OWNER.
    role            text NOT NULL CONSTRAINT organisation_invitations_role_check
                    CHECK (role IN ('MANAGER', 'AGENT', 'STAFF')),
    job_title       text CONSTRAINT organisation_invitations_job_title_length
                    CHECK (job_title IS NULL OR char_length(job_title) BETWEEN 1 AND 80),
    -- SHA-256 of the token in the emailed link. The token itself is never stored.
    token_hash      bytea NOT NULL CONSTRAINT organisation_invitations_token_hash_key UNIQUE,
    status          text NOT NULL DEFAULT 'PENDING' CONSTRAINT organisation_invitations_status_check
                    CHECK (status IN ('PENDING', 'ACCEPTED', 'CANCELLED', 'REPLACED')),
    invited_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    -- The inviter's name when they invited, shown on the invitation page.
    invited_by_name text NOT NULL,
    expires_at      timestamptz NOT NULL,
    accepted_at     timestamptz,
    accepted_by     uuid REFERENCES users (id) ON DELETE SET NULL,
    cancelled_at    timestamptz,
    cancelled_by    uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT organisation_invitations_expiry_check CHECK (expires_at > created_at),
    CONSTRAINT organisation_invitations_accepted_check CHECK ((status = 'ACCEPTED') = (accepted_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS organisation_invitations_one_pending_key
    ON organisation_invitations (organisation_id, email) WHERE status = 'PENDING';
CREATE INDEX IF NOT EXISTS organisation_invitations_organisation_idx
    ON organisation_invitations (organisation_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- The team's audit trail
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS organisation_audit_events (
    id              uuid PRIMARY KEY,
    organisation_id uuid NOT NULL REFERENCES organisations (id) ON DELETE CASCADE,
    actor_user_id   uuid REFERENCES users (id) ON DELETE SET NULL,
    -- The actor's name at the time, so a record stays readable after they leave.
    actor_name      text NOT NULL DEFAULT '',
    action          text NOT NULL CONSTRAINT organisation_audit_events_action_check CHECK (action IN (
                        'INVITATION_CREATED', 'INVITATION_CANCELLED', 'INVITATION_ACCEPTED',
                        'JOIN_REQUEST_APPROVED', 'JOIN_REQUEST_DECLINED', 'JOIN_REQUEST_CHANGES_REQUESTED',
                        'MEMBER_ROLE_CHANGED', 'MEMBER_JOB_TITLE_CHANGED', 'MEMBER_SUSPENDED',
                        'MEMBER_REACTIVATED', 'MEMBER_REMOVED', 'REQUEST_ASSIGNED', 'REQUEST_UNASSIGNED')),
    target_type     text NOT NULL CONSTRAINT organisation_audit_events_target_type_check
                    CHECK (target_type IN ('MEMBER', 'INVITATION', 'JOIN_REQUEST', 'PROPERTY_REQUEST')),
    target_id       uuid NOT NULL,
    -- A person's name, the invited address or a request reference.
    target_label    text NOT NULL DEFAULT '' CONSTRAINT organisation_audit_events_label_length
                    CHECK (char_length(target_label) <= 254),
    before_state    jsonb,
    after_state     jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS organisation_audit_events_organisation_idx
    ON organisation_audit_events (organisation_id, created_at DESC, id DESC);

CREATE OR REPLACE FUNCTION organisation_audit_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.organisation_id IS DISTINCT FROM OLD.organisation_id
       OR NEW.actor_name IS DISTINCT FROM OLD.actor_name
       OR NEW.action IS DISTINCT FROM OLD.action
       OR NEW.target_type IS DISTINCT FROM OLD.target_type
       OR NEW.target_id IS DISTINCT FROM OLD.target_id
       OR NEW.target_label IS DISTINCT FROM OLD.target_label
       OR NEW.before_state IS DISTINCT FROM OLD.before_state
       OR NEW.after_state IS DISTINCT FROM OLD.after_state
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (NEW.actor_user_id IS DISTINCT FROM OLD.actor_user_id AND NEW.actor_user_id IS NOT NULL) THEN
        RAISE EXCEPTION 'organisation_audit_events rows are immutable';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS organisation_audit_events_no_update ON organisation_audit_events;
CREATE TRIGGER organisation_audit_events_no_update BEFORE UPDATE ON organisation_audit_events
    FOR EACH ROW EXECUTE FUNCTION organisation_audit_events_immutable();

-- ---------------------------------------------------------------------------
-- Appointments: a member's open appointments go back to the organisation
-- ---------------------------------------------------------------------------
ALTER TABLE appointment_timeline DROP CONSTRAINT IF EXISTS appointment_timeline_event_check;
ALTER TABLE appointment_timeline
    ADD CONSTRAINT appointment_timeline_event_check
    CHECK (event IN ('REQUESTED', 'CONFIRMED', 'RESCHEDULE_REQUESTED', 'CANCELLED', 'COMPLETED', 'NO_SHOW', 'ASSIGNED', 'UNASSIGNED'));
