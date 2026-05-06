# `/api/oauth/usage` schema (allowlisted fields only)

This file documents the **subset** of the private OAuth usage endpoint's
response that cctrack consumes. Per F2 S2.1 ruling, only fields cctrack
actively uses are listed here; account-shaped fields are deliberately
**not** documented here, including their names.

| Field path                | Type   | Used by                              | Notes                                            |
| ------------------------- | ------ | ------------------------------------ | ------------------------------------------------ |
| `five_hour.utilization`   | number | `usageprovider.Snapshot.FiveHourUtilizationPercent`   | Integer 0–100 representing 5-hour window utilization. Treated as percentage. |
| `seven_day.utilization`   | number | `usageprovider.Snapshot.SevenDayUtilizationPercent`   | Integer 0–100 representing 7-day window utilization. Treated as percentage. |

## Discovery posture

This file is the **committed allowlist**, not a transcript of the wire
response. It exists so that future schema drift (a field renamed or removed
from the actual response) is detectable in a code review without anyone
rereading raw response bodies.

If the live response is observed to include reset/end/remaining-time fields
alongside `utilization`, those would be candidates for adding to this
allowlist (and to `Snapshot`) — but that decision belongs to a follow-on
slice (S2.2: anchor integration) and requires explicit EM review of which
fields are truly time-state vs. account-shaped.

## How to refresh this allowlist

1. Run the build-tagged probe locally:

       go run -tags discovery ./cmd/usage-probe

2. Inspect the sanitized stdout. The probe prints field names + JSON types
   only; values are elided for everything except the two allowlisted
   utilization paths. Names matching a defensive substring allowlist of
   generic auth/account vocabulary are redacted in the output (the list
   is in `cmd/usage-probe/main.go` as enforcement-only logic).

3. **Do not paste probe output into chat or commit it.** If a candidate
   new field is observed, add it to the table above by hand with an
   explicit note about why surfacing its value is non-sensitive.

## Hard rules (binding per F2 S2.1 verifier bars)

- **Allowlist-only**: only fields named here are parsed by `usageprovider`.
- **Fail-closed on used fields**: if `five_hour.utilization` or
  `seven_day.utilization` is missing from a 200 response, the adapter
  returns `ErrSchemaDrift` rather than partial data.
- **Ignore unknown extras**: extra response fields (whose names are not
  documented here) MUST NOT be treated as drift; the parser ignores them.
- **No raw response in logs/commits/chat**: not in error strings, not in
  test fixtures, not in commit messages, not in chat channels.
