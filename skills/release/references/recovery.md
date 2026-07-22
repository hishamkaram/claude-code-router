# Release Recovery

## Draft Release Failed

If the tag workflow fails before publishing a public release:

1. Keep the tag in place unless the tagged commit is wrong.
2. Fix the release workflow or packaging issue on `main`.
3. Move the tag only when the release commit itself must change, and record why
   in the acceptance file.
4. Rerun the tag workflow. Draft publication is idempotent when assets and
   checksums match the tag.

## Draft Exists, Verification Failed

- Do not promote.
- Fix the issue, rerun the tag workflow, and replace only artifacts produced by
  the workflow.
- Keep Homebrew unchanged.

## Public Release Failed After Promotion

- If GitHub Release publication succeeded but Homebrew failed, rerun promotion.
  The formula script exits successfully when the tap already matches the tag.
- If a public asset is wrong, publish a corrective patch release. Do not mutate
  already-consumed stable assets unless the project owner explicitly directs it.

## Homebrew Mismatch

Run `scripts/publish-homebrew-formula.sh vX.Y.Z` with `HOMEBREW_TAP_TOKEN`.
The script recomputes the source archive SHA-256 and only writes when content
differs.
