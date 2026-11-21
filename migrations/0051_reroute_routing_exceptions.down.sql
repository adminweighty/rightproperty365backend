-- The resolution of rerouted requests (RECEIVED) is dropped: the old rule
-- allows it on routing exceptions only.
UPDATE property_requests SET exception_resolved_at = NULL, exception_resolved_by = NULL, exception_resolution_note = NULL
WHERE status <> 'ROUTING_EXCEPTION' AND exception_resolved_at IS NOT NULL;
ALTER TABLE property_requests DROP CONSTRAINT IF EXISTS property_requests_exception_resolved_check;
ALTER TABLE property_requests ADD CONSTRAINT property_requests_exception_resolved_check
    CHECK (exception_resolved_at IS NULL OR status = 'ROUTING_EXCEPTION');
