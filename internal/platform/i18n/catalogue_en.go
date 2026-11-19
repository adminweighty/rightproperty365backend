package i18n

// catalogueEN is the reference catalogue: every key exists here. Keys are
// stable (clients never see them); the text may be reworded freely.
var catalogueEN = map[string]string{
	// The error envelope's shared messages (httpx).
	"error.not_found":              "The requested resource was not found.",
	"error.method_not_allowed":     "This method is not allowed for the requested resource.",
	"error.unsupported_media_type": "Content-Type must be application/json.",
	"error.payload_too_large":      "The request body is too large.",
	"error.invalid_json":           "The request body is not valid JSON for this endpoint.",
	"error.single_json_object":     "The request body must contain a single JSON object.",
	"error.internal":               "Something went wrong on our side. Please try again.",
	"error.timeout":                "The request took too long. Please try again.",
	"error.validation_failed":      "Some fields need your attention.",
	"error.unauthenticated":        "You need to sign in to do this.",
	"error.forbidden":              "You do not have permission to do this.",

	// Field messages used by the legal and tax routes.
	"field.required":       "is required",
	"field.too_long":       "must be at most {max} characters",
	"field.unknown_market": "is not a known market code",
	"field.no_html":        "must not contain HTML",

	// Legal documents (API 0.43.0).
	"legal.kind.TERMS":              "Terms and Conditions",
	"legal.kind.PRIVACY":            "Privacy Policy",
	"legal.kind.COOKIES":            "Cookie Policy",
	"legal.kind.REFUND":             "Refund Policy",
	"legal.kind.EULA":               "End User Licence Agreement",
	"legal.kind.AGENT_TERMS":        "Terms for Agents and Professionals",
	"legal.error.not_found":         "This legal document is not available.",
	"legal.error.preview_forbidden": "Only administrators can preview draft legal documents.",
	"legal.error.market_forbidden":  "You can only manage the legal documents of your own market.",
	"legal.error.immutable":         "A published or retired version cannot be changed. Create a new version instead.",
	"legal.error.exists":            "A version with this label already exists for this document, market and language.",
	"legal.error.not_acceptable":    "This version cannot be accepted: it is retired or not yet in effect.",
	"legal.error.version_not_found": "This version does not exist.",
	"tax.error.not_found":           "This tax rule does not exist.",
	"tax.error.overlap":             "Another active rule for this market and use covers part of the same period.",
	"tax.error.retired":             "This tax rule is already retired.",
}
