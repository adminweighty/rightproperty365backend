# Adding a market (country)

No code release is needed (BR-001). Everything happens in the administrators' area, as SUPER_ADMIN or
PLATFORM_ADMIN; a MARKET_ADMIN can prepare their own market but cannot change its status. Every step is in the admin
audit log.

1. **Provision it.** **Administration → Countries → add.** Give the ISO country code, name, main currency (ISO 4217)
   and time zone (IANA, for example `Africa/Johannesburg`). It starts **PLANNED**: hidden from every public list and
   from new listings.
2. **Packages.** **Administration → Listing packages:** create the market's packages (prices in its currency, active
   listing limits, billing terms). Then on the country, choose the **default listing package**.
3. **Legal documents.** **Administration → Legal documents:** publish the Terms of service and Privacy policy for the
   market. Without them the market falls back to the GLOBAL documents, which readiness reports as a warning.
   Content is the owner's and counsel's; the platform only versions and publishes it.
4. **Tax rules.** **Administration → Tax rules:** add the market's tax rates with their effective dates, if invoices
   there carry tax.
5. **Optional settings.** Extra permitted currencies (up to 5; listing prices, ledgers, maintenance and tenancies
   accept them), and a public **notice** (up to 280 characters, shown on search results).
6. **Reference data.** **Administration → Reference data:** add regions, cities and suburbs if the market should
   offer pick lists. A country without reference places accepts free-text places.
7. **Activate.** Set the status to **ACTIVE**. The API refuses (`409 MARKET_NOT_READY`, with the list of problems)
   unless:
   - the time zone loads;
   - the currency is ISO-shaped;
   - the default package is an active package of that market;
   - the Terms and the Privacy policy resolve.
8. **Staff.** Give a MARKET_ADMIN role for the new market to whoever runs it (an administrator account with that
   role, created with `cmd/adminctl`).
9. **Check:** the country appears in public country lists, a test listing in it goes through review, and prices show
   in its currency.

To take a market out of service, set it to **SUSPENDED**: it disappears from public lists again; existing records
stay with their owners. A public country never goes back to PLANNED.
