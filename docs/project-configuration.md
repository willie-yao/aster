# Project configuration

Each consumer repository owns a strict `project.yaml`. Unknown fields are errors,
so use the names documented here and in the annotated
[`project.reference.yaml`](../configs/example/project.reference.yaml).

Start with [`configs/example/project.yaml`](../configs/example/project.yaml), then
add only the optional sections you need.

## Required fields

```yaml
id: myproject
name: "My Project"

testgrid:
  dashboard: "sig-myproject-periodics"

storage:
  provider: gcs
  bucket: kubernetes-ci-logs

branding:
  title: "My Project Prow Dashboard"
  base_path: "/myproject-dashboard"
  site_url: "https://my-org.github.io/myproject-dashboard"
  source_repo:
    owner: my-org
    name: myproject
```

| Field | Purpose |
| --- | --- |
| `id` | Stable lowercase identifier used in cache keys and logs. |
| `name` | Human-readable project name. |
| `storage` | Artifact backend and bucket. |
| `branding` | Site identity, base path, URL, and source repository. |
| `testgrid.dashboard` | Required for the default `testgrid` discovery source. Omit it only when using bucket discovery. |

For GitHub Pages, `branding.base_path` is `/<host-repo>` and `site_url` is the
full Pages URL. For Kubernetes-native deployments, use `/` and the ingress URL.

## Storage

The engine supports three providers:

```yaml
# Native Google Cloud Storage.
storage:
  provider: gcs
  bucket: kubernetes-ci-logs

# A gcsweb gateway, including S3-backed Prow installations.
storage:
  provider: gcsweb
  bucket: my-prow
  base: "https://gcsweb.example.net/s3"
  prow_base: "https://prow.example.net/view/s3"

# A downloaded artifact tree for tests or offline runs.
storage:
  provider: local
  base: "/absolute/path/to/artifacts"
  web_base: "https://artifacts.example.net" # optional public link root
```

`web_base` overrides generated artifact links. `prow_base` overrides Prow build
links. The local provider is intended for tests and offline fetches. Set
`web_base` before publishing data produced from a local tree.

## Job discovery

The default source reads Kubernetes test-infra job configurations and keeps jobs
whose `testgrid-dashboards` annotation contains `testgrid.dashboard`.

For another Prow installation, discover directly from the artifact bucket:

```yaml
discovery:
  source: bucket
  job_filters:
    - "integration-"
```

`job_filters` are optional substring filters. Omit them to include every job in
the bucket.

Periodics are included by default. Add presubmits with:

```yaml
source:
  include_presubmits: true
```

## Categories

Categories are optional. Without them, the dashboard renders one flat job grid.
Rules are checked in order with case-insensitive substring matching.

```yaml
categories:
  - match: "conformance"
    id: conformance
    label: Conformance
  - match: "e2e"
    id: e2e
    label: E2E

category_display_order: [e2e, conformance, other]
```

The first matching rule wins. Unmatched jobs use the reserved `other` category.

## AI provider and analysis

AI is optional at the fetcher level. When it is enabled, the endpoint, model,
token, and a non-empty `prompts/system.md` are required. The endpoint must support
OpenAI-style function calling.

Provider coordinates can be committed in `project.yaml`:

```yaml
ai:
  endpoint: "https://api.example.net/v1/chat/completions"
  model: "model-id"
```

For a public consumer repo, omit those fields and pass `AI_ENDPOINT` and
`AI_MODEL` through the deployment environment. The YAML values win when both are
set.

Optional provider and loop fields:

```yaml
ai:
  headers:
    X-Routing-Key: "public-routing-value"
  concurrency: 1
  tools: [filesystem, k8s]
  max_iters: 15
  timeout: 5m
  min_tool_calls: 2
  min_gcs_bytes: 0
  single_tool_call: false
  critique:
    max_retries: 2
```

Do not commit credentials under `ai.headers`. `AI_TOKEN` is the supported bearer
token channel. Providers that require a secret in another header, such as an
Azure `api-key`, need a trusted proxy or a customized deployment that injects the
header without storing it in public YAML. See [AI providers](ai-providers.md).

The in-process loop reads all fields above. The experimental Orka backend reads
only the project id, prompt, storage settings, and `ai.tools`; its execution
settings live under Helm `orka.*` values.

## Skills

Consumer diagnostic recipes are YAML files under `skills/*.yaml`. Their presence
is the opt-in; there is no `skills.enabled` field. See [Skills](skills.md).

## Optional write features

Automatic issue filing is configured under top-level `issues`. Agent-proposed
fix PRs are configured under `ai.fix_prs`.

```yaml
issues:
  enabled: true
  repo: {owner: my-org, name: ci-tracking}

ai:
  fix_prs:
    enabled: true
    author_name: "Jane Maintainer"
    author_email: "jane@example.com"
    dry_run: true
    verify:
      enabled: true
      commands:
        - go build ./...
        - go vet ./...
      timeout: 10m
```

These features also require their write tokens and deployment prerequisites. See
[GitHub issues](github-issues.md) and [Fix PRs](fix-prs.md).

## Validate a config

A discovery-only fetch validates the strict schema and confirms that jobs are
found without making AI calls:

```bash
./bin/fetcher -project-dir=../my-consumer -ai=false -builds=1
```

Use the generated `data/dashboard.json` job count as the success check.
