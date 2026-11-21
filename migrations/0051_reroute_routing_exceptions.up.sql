-- 0051: reassigning a routing exception (API 0.49.0, FR-061). An
-- administrator gives a request nobody could receive to an organisation of
-- its market: it becomes RECEIVED and keeps its resolution (who, when,
-- note), so the resolution may now sit on a RECEIVED request too. Such a
-- request stays in the administrators' list of resolved exceptions.

ALTER TABLE property_requests DROP CONSTRAINT IF EXISTS property_requests_exception_resolved_check;
ALTER TABLE property_requests ADD CONSTRAINT property_requests_exception_resolved_check
    CHECK (exception_resolved_at IS NULL OR status IN ('ROUTING_EXCEPTION', 'RECEIVED'));
