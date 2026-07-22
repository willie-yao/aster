# Orka compatibility worker matrix

The dashboard publishes a patched Orka AI worker for the experimental analysis
backend. Each image is built from one pinned Orka commit and one dashboard
commit. Moving tags are not published.

## Current contract

| Field | Compatibility v6 |
| --- | --- |
| Orka repository | `https://github.com/orka-agents/orka.git` |
| Orka commit | `1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254` |
| Orka Go version | `1.26.2` |
| Patch | `ai-worker-convergence.patch` |
| Patch SHA-256 | `b118176958f647207291bbcb5ec9d470e06dc1c31776e2f509a8a74dfca6b44d` |
| Worker Dockerfile | `workers/ai/Dockerfile` from the pinned Orka commit |
| Published platform | `linux/amd64` |
| Workflow | `.github/workflows/orka-compat-image.yml` |

`compatibility.env` is the machine-readable source for the pinned values. The
build fails when its patch checksum, source commit, or tag inputs do not match.

Compatibility v6 retains the provider API observability, Copilot model fallback,
strict final schema, and bounded repair budget from v5. It splits the 20-call
evidence budget into 16 initial investigation calls plus four calls reserved for
validation repair. When every missing evidence group has exact candidates, a
resolved `read_artifact` Tool is available, and the queue fits the remaining call
and iteration budgets, the worker stores the groups in validation order. It then
advertises only `read_artifact` for the current group and selects one normalized
candidate path. A correct model-generated read is preserved. Empty responses,
premature finalization, wrong readers, wrong paths, and repeated paths are
replaced with a synthetic call through the normal guarded Tool execution path.
Each successful or cached read advances exactly one group. Finalization Tools
return only after the queue is complete, leaving a final repaired-submission
turn. Incomplete or impossible plans retain the existing validation failure
behavior, and broad investigation never resumes after finalization starts.

Responses requests set `store: false`; completed model events and worker logs
include the negotiated API mode and response ID. The dashboard ingestor can
enforce `auto`, `responses`, or `chat_completions` without adding fields to
Orka's Provider CRD.

## Image identity

The workflow publishes this tag shape:

```text
v6-orka-<full-orka-commit>-dashboard-<full-dashboard-commit>
```

For this contract the image repository is:

```text
ghcr.io/willie-yao/prow-ai-dashboard/orka-ai-worker
```

The tag contains both source revisions and is never overwritten. A rerun accepts
an existing tag only when its runtime contract labels, SLSA provenance, and SPDX
SBOM match the exact Orka commit, dashboard commit, and patch checksum. This lets
a run reconstruct a missing summary or artifact after the registry push already
succeeded. Any mismatch or registry inspection error fails closed.

The workflow summary and `orka-compat-image-<dashboard-commit>` artifact record
the registry digest and source identities. Deploy by digest when the surrounding
Orka installation supports a full image reference.

## Find the current deployable coordinate

Open the [successful main-branch compatibility runs](https://github.com/willie-yao/prow-ai-dashboard/actions/workflows/orka-compat-image.yml?query=branch%3Amain+is%3Asuccess) and select the
newest run whose `publish` job completed. Its workflow summary shows the exact
image tag and digest. The `orka-compat-image-<dashboard-commit>` artifact contains
the same values as JSON. Pull-request validation artifacts are marked
`published: false` and are not deployment coordinates.

## Validation

Every pull request that changes the compatibility files:

1. Checks out the exact Orka commit.
2. Verifies and applies the patch to a clean checkout.
3. Runs the focused compatibility tests.
4. Runs the OpenAI provider and shared LLM package tests.
5. Runs the complete `./workers/ai` package with the race detector.
6. Runs the complete `./workers/ai` package test suite.
7. Renders the pinned Orka chart with both immutable-tag and digest overrides.
8. Builds `workers/ai/Dockerfile` for `linux/amd64` without publishing.

A push to `main` must pass the same validation job. A separate package-write
job then rebuilds and pushes the image, SBOM, provenance, and digest record. Pull
request jobs receive no package-write permission.

Local metadata validation:

```bash
make orka-compat-check
```

Full local test and image build:

```bash
make orka-compat-image \
  ORKA_COMPAT_IMAGE=ghcr.io/willie-yao/prow-ai-dashboard/orka-ai-worker:local
```

The full build requires Docker and network access to the pinned Orka repository.

## Deploy with Orka Helm

The current Orka chart accepts the AI worker through
`workers.ai.image.repository` and `workers.ai.image.tag`.

Immutable combined tag:

```yaml
workers:
  ai:
    image:
      repository: ghcr.io/willie-yao/prow-ai-dashboard/orka-ai-worker
      tag: v6-orka-1b6f6f74c8cdf5e3ccfe92d0a7ed03a571670254-dashboard-<dashboard-commit>
      pullPolicy: IfNotPresent
```

Strict digest pinning with the current `repository:tag` chart template:

```yaml
workers:
  ai:
    image:
      repository: ghcr.io/willie-yao/prow-ai-dashboard/orka-ai-worker@sha256
      tag: <64-character-digest-without-the-sha256-prefix>
      pullPolicy: IfNotPresent
```

The second form renders a standard
`ghcr.io/.../orka-ai-worker@sha256:<digest>` image reference. Obtain the digest
from the workflow summary or compatibility artifact.

GHCR creates new container packages as private unless account settings say
otherwise. Make this package public before using it with the pinned Orka chart,
or provision a pull Secret on the dynamic AI-worker ServiceAccount separately.
The pinned chart does not expose worker `imagePullSecrets` values.

## Updating the contract

Do not move an existing compatibility tag to new content. For an Orka update:

1. Bump `COMPATIBILITY_VERSION` and update the pinned values in
   `compatibility.env`.
2. Recreate the patch against the new pinned Orka commit.
3. Update the patch SHA-256 and this matrix.
4. Run the complete compatibility workflow.
5. Deploy the new tag or digest as an explicit Orka Helm change.
6. Keep the prior image available for rollback.

If the worker changes merge upstream, create a new contract without the obsolete
patch rather than silently changing the prior contract.
