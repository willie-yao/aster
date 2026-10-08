# Example project AI prompt addendum

Replace this example's unresolved architecture and artifact details with known project facts. The engine supplies universal Prow guidance and the response format. See `docs/writing-prompts.md` for the manual starter.

## Architecture

Unresolved. Do not assume component relationships that the project has not documented.

## Diagnostic lifecycle

Unresolved. Use timestamped build evidence to identify the phase that failed.

## Test and job flavors

Unresolved. Identify the actual job flavor before applying flavor-specific guidance.

## Artifact layout

Project-specific paths are unresolved. Use files actually listed in the build's artifact tree.

## Common failure patterns

No project-specific patterns are established. Do not treat a plausible cause as a known recurring failure.

## Transient classification

No project-supported transient rules are established. Do not classify a failure as transient from a generic error signature alone.

## Triage order

Start with the failing test and build metadata. Inspect the earliest relevant error and separate it from later cleanup or timeout noise. Compare a passing build when available.

## Relevant source repositories

Unresolved. Cite only repositories and paths established by source or artifact evidence.

## Unresolved details

The tested components, project-specific artifact paths, job flavors, and recurring failure patterns need maintainer input.
