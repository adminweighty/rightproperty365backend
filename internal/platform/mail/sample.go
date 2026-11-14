package mail

import (
	"net/url"
	"time"
)

// sampleTime is fixed so previews and golden files do not change from run to run.
var sampleTime = time.Date(2026, 11, 14, 9, 5, 0, 0, time.UTC)

// SampleData returns realistic, fixed data for a template. It is used by
// cmd/mailpreview and by the golden-file tests. The token is obviously fake.
func SampleData(name string) Data {
	d := Data{Name: "Tariro Moyo", When: sampleTime}
	const sampleToken = "SAMPLE-TOKEN-not-a-real-credential"
	const preview = "Hello, is this house still available? I would like to view it on Saturday morning if possible. My budget is around USD 250,000."
	switch name {
	case "verify-email":
		d.Path, d.Query = "/verify-email", url.Values{"token": {sampleToken}}
	case "reset-password":
		d.Path, d.Query = "/reset-password", url.Values{"token": {sampleToken}}
	case "password-changed", "account-exists":
		d.Path = "/sign-in"
	case "welcome":
		d.Path = "/account"
		d.Roles = []Role{{Name: "Buyer or renter"}, {Name: "Landlord or property manager", Pending: true}, {Name: "Estate agent", Pending: true}}
	case "admin-sign-in":
		d.Name, d.Path = "Sam Admin", "/admin/sign-in"
	case "property-request-received":
		d.Path, d.Reference, d.RequestType = "/", "RP-7K2M9Q4D", "Viewing request"
		d.ListingTitle, d.ListingRef = "Four-bedroom family home with garden", "RP-ZW-10001"
	case "property-request-new":
		d.Name, d.Path, d.Reference, d.RequestType = "Tendai Ncube", "/agent/requests", "RP-7K2M9Q4D", "Viewing request"
		d.ListingTitle, d.ListingRef, d.Preview = "Four-bedroom family home with garden", "RP-ZW-10001", SafePreview(preview, 140)
	case "business-enquiry-received":
		d.Path, d.Reference, d.ProfileName, d.Service = "/", "BE-4H8N2RXT", "Sample Plumbers", "Burst pipes and leaks"
	case "business-enquiry-new":
		d.Name, d.Path, d.Reference, d.ProfileName, d.Service = "Sample Plumbers Owner", "/agent/requests", "BE-4H8N2RXT", "Sample Plumbers", "Burst pipes and leaks"
		d.Preview = SafePreview("The geyser is leaking into the ceiling. Can someone come this week? Call me on +263 77 123 4567 or write to tariro@example.com.", 140)
	case "onboarding-submitted":
		d.Name, d.Path, d.RoleName = "Sample Plumbers Owner", "/onboarding", "Service provider"
	case "onboarding-approved":
		d.Name, d.Path, d.RoleName, d.ProfileName = "Sample Plumbers Owner", "/directory/service-providers/sample-plumbers", "Service provider", "Sample Plumbers"
	case "onboarding-changes-requested":
		d.Name, d.Path, d.RoleName = "Sample Plumbers Owner", "/onboarding", "Service provider"
		d.Reason = "The proof of address is older than three months. Please upload a recent one."
	case "onboarding-rejected":
		d.Name, d.Path, d.RoleName = "Sample Plumbers Owner", "/account", "Service provider"
		d.Reason = "We could not confirm the business registration with the documents provided."
	case "request-update":
		d.Path, d.Reference, d.StatusText = "/account", "RP-7K2M9Q4D", "The agent has replied"
	case "listing-expiring":
		d.Name, d.Path, d.Days = "Tendai Ncube", "/account", 7
		d.ListingTitle, d.ListingRef, d.ExpiresOn = "3-bedroom house in Borrowdale", "0195c0de-0000-7000-8000-000000000001", sampleTime.AddDate(0, 0, 7)
	case "listing-expired":
		d.Name, d.Path = "Tendai Ncube", "/account"
		d.ListingTitle, d.ListingRef, d.ExpiresOn = "3-bedroom house in Borrowdale", "0195c0de-0000-7000-8000-000000000001", sampleTime
	case "listing-relisted":
		d.Name, d.Path = "Tendai Ncube", "/account"
		d.ListingTitle, d.ListingRef, d.ExpiresOn = "3-bedroom house in Borrowdale", "0195c0de-0000-7000-8000-000000000001", sampleTime.AddDate(0, 0, 30)
	case "renewal-invoice":
		d.Name, d.Path, d.InvoiceNumber, d.Amount, d.DueOn = "Rudo Moyo", "/account", "INV-2026-00042", "USD 25.00", sampleTime.AddDate(0, 0, 14)
	case "payment-received":
		d.Name, d.Path, d.InvoiceNumber, d.Amount = "Rudo Moyo", "/account", "INV-2026-00042", "USD 25.00"
	case "account-deletion-requested":
		d.Path, d.DeletionOn = "/sign-in", sampleTime.AddDate(0, 0, 30)
	case "account-deletion-completed":
		d.Path = "/"
	}
	return d
}
