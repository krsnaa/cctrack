# `/api/oauth/usage` schema (allowlisted fields only)

This file documents the **subset** of the private OAuth usage endpoint's
response that cctrack consumes. Per F2 S2.1 ruling, only fields cctrack
actively uses are listed here; account-shaped fields are deliberately
**not** documented here, including their names.

| Field path                | Type   | Used by                              | Notes                                            |
| ------------------------- | ------ | ------------------------------------ | ------------------------------------------------ |
| `five_hour.utilization`   | number | `usageprovider.Snapshot.FiveHourUtilizationPercent`   | Integer 0–100 representing 5-hour window utilization. Treated as percentage. |
| `seven_day.utilization`   | number | `usageprovider.Snapshot.SevenDayUtilizationPercent`   | Integer 0–100 representing 7-day window utilization. Treated as percentage. |
| `five_hour.resets_at`     | string | `usageprovider.Snapshot.FiveHourResetsAt`             | RFC3339 / RFC3339Nano timestamp; the moment the rolling 5-hour window resets. Parser normalizes to UTC; non-RFC3339 formats fail closed with `ErrSchemaDrift`. |
| `seven_day.resets_at`     | string | `usageprovider.Snapshot.SevenDayResetsAt`             | RFC3339 / RFC3339Nano timestamp; the moment the rolling 7-day window resets. Parser normalizes to UTC; non-RFC3339 formats fail closed with `ErrSchemaDrift`. |

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

2. Inspect the stdout. Per T2.1.5.1's inverse-allowlist model, the probe
   emits one line per allowlisted exact path only — `name + type +
   presence` for each of the four cctrack-used fields. Every other
   response field name and value is silently dropped (no `<redacted>`
   markers, no value previews, no nested-object enumeration). An
   allowlisted path that is missing from the response is reported as
   `MISSING`; one with the wrong JSON type is reported as
   `TYPE_MISMATCH`. The probe exits with status 3 on either condition
   so a future CI gate can detect schema drift mechanically.

3. **The output above is the complete signal.** If you need to expand
   the allowlist, the binding contract is in this file (SCHEMA.md);
   `cmd/usage-probe/walk.go` and `internal/usageprovider/provider.go`
   must both be kept aligned to it. Verifier checks for drift on every
   slice that touches usageprovider.

4. **Do not paste probe output into chat or commit raw stdout.** Even
   though the probe by construction does not emit non-allowlisted
   names, treat the output as a working artifact, not a documentation
   surface — when extending the allowlist, edit SCHEMA.md by hand with
   an explicit note about why surfacing the new field is non-sensitive.

## Hard rules (binding per F2 S2.1 + S2.2 verifier bars)

- **Allowlist-only**: only fields named here are parsed by `usageprovider`.
- **Fail-closed on used fields**: if any of `five_hour.utilization`,
  `seven_day.utilization`, `five_hour.resets_at`, or `seven_day.resets_at`
  is missing from a 200 response, the adapter returns `ErrSchemaDrift`
  rather than partial data.
- **`resets_at` parse strictness**: only RFC3339 / RFC3339Nano formats
  accepted. Non-conforming values fail closed with `ErrSchemaDrift`. The
  probe is NOT widened to print value formats; if a different format is
  observed in live smoke, escalate to EM with no values pasted.
- **Ignore unknown extras**: extra response fields (whose names are not
  documented here) MUST NOT be treated as drift; the parser ignores them.
- **No raw response in logs/commits/chat**: not in error strings, not in
  test fixtures, not in commit messages, not in chat channels.
