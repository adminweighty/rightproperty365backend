# Administrator guide

For the people who run RightProperty365 from the administration area (website, `/admin`). Covers everything up to
API 0.61.0. The administration area is web-only: the mobile app has no admin screens.

## Signing in

- Use **`/admin/sign-in`**, not the public sign-in. An administrator account is separate from any customer or agent
  account, and a platform role does nothing on a customer session.
- After your password, enter the code we email you. If an authenticator app is set up, enter its 6-digit code too.
- Set up your authenticator under **Administration → Security**, and keep the recovery codes somewhere safe. Using a
  recovery code emails you a warning. In production an authenticator is required: until you add one, you can only
  reach the set-up page.
- Sessions end after 12 hours, or after 30 minutes without activity.
- Administrator accounts are created by the technical team with the `adminctl` tool. Nobody can become an
  administrator through the website.

## Roles

Each role sees only the pages it can use. **Super Administrator can do everything.** A Market Administrator works
only on their own country.

| Page | Super Admin | Platform Admin | Market Admin | Finance Admin | Content Admin | Moderator | Support |
| --- | :-: | :-: | :-: | :-: | :-: | :-: | :-: |
| Accounts (find, view) | ✓ | ✓ | | | | | ✓ |
| Accounts (suspend, reactivate, delete) | ✓ | ✓ | | | | | |
| Verifications (professionals) | ✓ | ✓ | own country | | | ✓ | |
| Listings review | ✓ | ✓ | own country | | | ✓ | |
| Reported listings | ✓ | ✓ | own country | | | ✓ | |
| Development reviews | ✓ | ✓ | own country | | | ✓ | |
| Reviews of service providers | ✓ | | | | ✓ | ✓ | |
| Requests nobody could receive | ✓ | ✓ | own country | | | | ✓ |
| Subscriptions, invoices, online payments (view) | ✓ | ✓ | | ✓ | | | ✓ |
| Subscriptions, invoices (act), refunds, payment exports | ✓ | | | ✓ | | | |
| Subscription plans | ✓ | | | ✓ | | | |
| Listing packages | ✓ | ✓ | own country | | | | |
| Featured and sponsored placements | ✓ | ✓ | own country | ✓ | | | |
| Countries | ✓ | ✓ | own country (not its status) | | | | |
| Tax rules | ✓ | | | ✓ | | | |
| Legal documents | ✓ | | own country | | ✓ | | |
| Lease templates (platform) | ✓ | | | | ✓ | | |
| Leases and agreements (oversight, read-only) | ✓ | | | | | | ✓ |
| Reference data (lists) | ✓ | ✓ | own country | | ✓ | | |
| Reports and spreadsheets | ✓ | ✓ (people export) | | ✓ (no people export) | | | |
| Analytics | ✓ | ✓ | own country | | | | |
| Email delivery | ✓ | ✓ | | | | | ✓ |
| Upload scanning | ✓ | ✓ | | | | | |
| Audit log | ✓ | ✓ | | | | | |

There is no separate "Finance" or "Content" section: those roles use the pages marked for them above.

## Everyday tasks

### Approve a professional (agent, agency, landlord, developer…)

**Verifications** → open the application → check the documents (they open only after the virus check, if scanning is
on) → **Approve**, **Request changes** (say what is missing) or **Reject** (with a reason). The applicant is emailed.
Approving creates their organisation, membership or public profile. You cannot review an application you are part of.

### Review a listing

**Listings** → open a listing waiting for review → compare the changes (an edit shows before and after) →
**Approve**, **Request changes** or **Reject**, with a reason for the last two. Approved listings go public for 90 days;
an edit stays private until approved. The page shows flags (possible duplicate, contact details in the text, burst of
listings, all capitals): check them, they are hints, not verdicts. You cannot review your own organisation's listing.

### Handle a reported listing

**Reported listings** → open a report → **Dismiss**, **Warn the advertiser** (they get an email with your note) or
**Take down** (the listing leaves public view at once, with your reason). Reports are anonymous to the advertiser.

### Moderate reviews

**Reviews** → publish or reject reviews of service providers; handle reported reviews.

### Requests nobody could receive

Sometimes a customer's request has no one to receive it (the agent left, the agency was removed). These appear in
**Requests nobody could receive**. Open one and **reassign** it to an organisation or a member, or resolve it with a
note. The customer was told it was received; nothing was lost.

### Money: subscriptions, invoices and payments

- **Subscriptions:** record a payment, reject or cancel a subscription.
- **Invoices:** draft, issue, record payments, issue a credit note, void, mark disputed. Issued invoices cannot be
  edited: correct them with a credit or void and reissue. Invoice numbers are sequential per country.
- **Online payments:** see payments made online, the ones held for review, refunds and the spreadsheet. **Online
  payment is switched off** until a payment provider is connected, so customers pay by the methods you record.
- **Subscription plans** and **Listing packages**: prices per country, active-listing limits, and the plans
  professionals can choose. Renewing a subscription never republishes an expired listing: the advertiser relists.
- **Tax rules:** tax rates per country with the dates they apply.

### Featured and sponsored placements

**Featured and sponsored** → give a listing a placement for a period, or end one. The public listing then shows a
"Featured" or "Sponsored" label and a disclosure line. Each placement is tied to the advertiser's active subscription, and its plan sets how many featured listings they may have.

### Countries

**Countries** → prepare a new country while it is PLANNED (currency, extra currencies, time zone, default package,
notice), then make it ACTIVE. Activation is refused until the country is ready, and the page lists what is missing.
The full steps are in the runbook `adding-a-market.md`.

### Legal documents

**Legal documents** → per country (or GLOBAL): write a draft, publish it, retire an old version. People are asked to
accept a new version of documents they must accept. Wording is the owner's and counsel's responsibility.

### Reference data

**Reference data** → the lists people choose from: property types, features, places (regions, cities, suburbs),
roles and more. Changes apply at once.

### Reports and analytics

- **Reports:** figures over a range of days, and spreadsheet downloads (people, subscriptions, payments). Exports
  may contain personal data: keep them private.
- **Analytics:** listing views, enquiries and top listings by country. The definitions are on the public page
  `/analytics-definitions`.

### Email delivery

**Email delivery** → see whether our email arrives: delivered, bounced, marked as spam, not sent (suppressed), refused.
Spikes are listed. After a permanent bounce or a spam complaint we stop emailing that address, except sign-in codes
and account notices. Look an address up under **Suppressed addresses**, and **Clear** it only when the person asks (a
reason is required and logged). If the page says "Delivery reports are off", the provider's webhook is not
configured.

### Upload scanning

**Upload scanning** → whether uploaded files are checked for viruses, files waiting, and files removed as unsafe. If it
says scanning is off, files are stored unchecked (the current staging setting).

### Accounts

**Accounts** → find a person; see their roles, organisations and subscriptions; **suspend**, **reactivate** or
**delete** (delete anonymises the account and it can never sign in again). Support can look but not change.

### Audit log

**Audit log** → every change an administrator made, with before and after, who and when. Nothing in it can be edited.

## What administrators cannot do

- Read a customer's messages to an agent, an agency's contact book, ledger or applications: those belong to the
  organisation. Administrators see them only where a page above says so.
- Change an issued invoice, or a posted ledger entry.
- Make someone an administrator from the website.
- Turn on push notifications, virus scanning, online payment or app links from the website: these need technical
  set-up (runbook `enabling-switched-off-features.md`).
