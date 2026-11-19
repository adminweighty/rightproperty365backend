package legal

import (
	"time"

	"github.com/google/uuid"
)

// SeedVersion is the version label of every seeded document: the web's
// current version of its own pages (src/lib/legal.ts), so the privacy
// versions registration records line up.
const SeedVersion = "2026-11-14"

// SeedCreator is the name seeded rows carry as their creator and publisher.
const SeedCreator = "System (development seed)"

// DraftBanner opens every placeholder body.
const DraftBanner = "> **DRAFT — awaiting legal review.** This placeholder is not the final text and is not legal advice."

type seedDoc struct{ kind, title, path string }

var seedDocs = []seedDoc{
	{KindTerms, "Terms and Conditions", "terms-and-conditions"},
	{KindPrivacy, "Privacy Policy", "privacy-policy"},
	{KindRefund, "Refund Policy", "refund-policy"},
	{KindEULA, "End User Licence Agreement", "eula"},
}

// SeedMarkets are the markets that get DRAFT copies of the GLOBAL placeholders.
var SeedMarkets = []string{"ZW", "GB"}

// PlaceholderBody is the body of a seeded document.
func PlaceholderBody(title, path string) string {
	return DraftBanner + "\n\n" +
		"The RightProperty365 " + title + " has not yet been reviewed by a lawyer. Until the reviewed text is published here, " +
		"the working text is the one on the website: https://rightproperty365.com/legal/" + path + "\n"
}

// SeedVersions returns the DEVELOPMENT AND STAGING documents (the memory
// stores and devseed.SeedPostgresLegalTax; migration 0045 inserts none, so
// production has nothing published): GLOBAL TERMS, PRIVACY, REFUND and EULA
// PUBLISHED (placeholders marked DRAFT in the body, with DraftNotice) and
// DRAFT copies for ZW and GB.
func SeedVersions(now time.Time) []Version {
	effective := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	var out []Version
	for _, d := range seedDocs {
		for _, market := range append([]string{MarketGlobal}, SeedMarkets...) {
			v := Version{
				ID:   uuid.NewSHA1(uuid.NameSpaceURL, []byte("rp365:legal:"+d.kind+":"+market+":en:"+SeedVersion)),
				Kind: d.kind, MarketCode: market, Lang: "en", Label: SeedVersion, EffectiveFrom: effective,
				Status: StatusDraft, Body: PlaceholderBody(d.title, d.path), DraftNotice: true, CreatedByName: SeedCreator,
				Summary:   "DRAFT copy of the GLOBAL placeholder for " + market + ", for the market-specific text from the lawyers.",
				CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
			}
			if market == MarketGlobal {
				at := effective
				v.Status, v.PublishedAt, v.PublishedByName = StatusPublished, &at, SeedCreator
				v.Summary = "Placeholder: DRAFT — awaiting legal review."
			}
			out = append(out, v)
		}
	}
	return out
}
