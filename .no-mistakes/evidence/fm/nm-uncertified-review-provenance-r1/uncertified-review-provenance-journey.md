# Uncertified review provenance (R5 option A persist-only)

End-user journey exercised against the live ReviewStep + DB APIs, matching how a replacement reviewer sees the next run.

## 1. Fixer commit persists a per-branch range

A review-step fixer commit advanced HEAD. The pipeline persisted one uncertified range for the branch (`from_sha` = pre-fix HEAD, `to_sha` = fixer commit). Lint and document fixer commits do not write this row.

See `persisted-uncertified-range.json`.

## 2. Next run's initial review is not cold

A new run on the same branch bound that range while `Fixing==false` and delivered `fixRoundProvenanceClause` in the generated review prompt, naming the persisted SHAs. It did not use the current-run "re-review after this run's automated fix round(s)" framing.

See `next-run-initial-review-prompt.txt` and `next-run-provenance-clause.txt`.

## 3. Rerun is not gated

`no-mistakes rerun --help` and `no-mistakes axi run --help` expose only the existing flags. There is no `--ack-uncertified-review` (or any other uncertified-review refusal gate). Rerun proceeds; the next initial review is just no longer blind.

See `rerun-help.txt` and `axi-run-help.txt`.

## 4. Fail-open and clear rules (executable tests)

- Missing git objects: bind logs a warning and continues without applying provenance (never blocks). See `missing-objects-warn-and-continue.json`.
- Parked, failed, and aborted reviews leave the persisted range in place.
- A completed review clears the range only when the approved head equals `to_sha` or is a descendant of it. A non-descendant approved head leaves the row.
