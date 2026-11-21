# Email bounce or complaint spike

**Signal:** alert `RP365EmailBounceSpike` or `RP365EmailComplaintSpike`, an ERROR log line `email delivery spike`
from the worker, or a spike card on **Administration → Email delivery**. This needs Resend's delivery webhook to be
configured (`RESEND_WEBHOOK_SECRET`); without it no bounce is ever recorded and this page stays empty.

Thresholds (worker, every 5 minutes, over the last hour):

- **Bounce spike:** at least 5 bounces that are also at least 5% of the email accepted.
- **Complaint spike:** at least 3 complaints that are also at least 0.1%.

## First 15 minutes

1. Open **Administration → Email delivery**. Check "Recent problems": which email kinds bounce (a sign-in code? a
   saved-search alert?) and whether one kind dominates.
2. Check what was sent in that hour. A batch to an old list, a new template, or a release that changed recipients
   is the usual cause.
3. If a single feature is sending to bad addresses, stop it: roll back the release, or pause the feature.
4. Complaints: read the template that was reported. Is it marketing-like? Does it honour the person's preferences?
5. Check the sending domain in the Resend dashboard: reputation, suppression list, DNS (SPF, DKIM, DMARC).

## What the system already does

- A **permanent** bounce or a complaint **suppresses** the address (its hash). It then gets no email except
  sign-in codes, verification, password and address changes, signing codes and account notices, so nobody is
  locked out.
- A transient bounce does not suppress.
- Suppressed messages are recorded as FAILED with reason `suppressed`. They are not dead letters and are not retried.

## Clearing a suppression

Only when the person has fixed their mailbox, or has asked to receive email again:
**Email delivery → Suppressed addresses**, look the address up, **Clear**, and give a reason. Clearing is in the
admin audit log. Never clear complaints in bulk.

## Afterwards

Write down the cause and the fix. If the threshold was noise, change it in code
(`internal/modules/emaildelivery`) and in `monitoring/prometheus-rules.yaml` together.
