# GitHub read authentication

Aster can renew GitHub App installation tokens in memory while the worker and server keep running. This covers Prow job discovery, scheduled source analysis, pull request triage, chat, escalation, source-link checks, and Fix source validation. AI provider credentials and GitHub write credentials have separate roles.

## Install the App on the read repositories

Grant the App these repository permissions:

| Permission | Used for |
| --- | --- |
| **Contents: Read-only** | Commits, trees, files, and source archives, including `kubernetes/test-infra` job discovery. |
| **Metadata: Read-only** | Repository metadata. GitHub includes this permission automatically. |
| **Pull requests: Read-only** | Pull request triage and escalation, including changed files. Needed on the repositories using these features. |

Select every repository Aster reads during installation. A CAPZ deployment normally needs `kubernetes-sigs/cluster-api-provider-azure` and `kubernetes/test-infra`, plus any other configured source repositories. Installing only on the consumer repository, such as `capz-aster`, does not grant access to those repositories. Public repositories also need to be selected for installation-token access. Public endpoints that permit anonymous reads do not expand an installation token's repository access.

Installation requires a repository administrator or organization owner. Organization policy can require owner approval. Obtain that approval for CAPZ and test-infra before enabling App authentication. Aster does not substitute an anonymous request or PAT when an App installation is missing. See GitHub's [installation requirements](https://docs.github.com/en/apps/using-github-apps/installing-a-github-app-from-a-third-party#requirements-to-install-a-github-app), [App access rules](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/differences-between-github-apps-and-oauth-apps#requesting-permission-levels-for-resources), and [endpoint permission reference](https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps).

Read tokens are narrowed to one repository and the read permission needed for a request. The existing App comment helper keeps its separate token and permission scope. If optional comments are enabled, that App also needs **Issues: Read and write** on `branding.source_repo`. Configuring App credentials for comments also selects App authentication for reads, so all read installations must be ready.

## Supply the credentials

Use the existing environment names `ASTER_APP_ID` and `ASTER_APP_PRIVATE_KEY`. The private key must be an RSA PEM generated for the App. Keep it in the organization's Secret manager.

For Kubernetes, provision an existing Secret in the Aster namespace containing those two keys, then reference it in `deploy/values.yaml`:

```yaml
githubApp:
  existingSecret: capz-aster-github-app
  appIDKey: ASTER_APP_ID
  privateKeyKey: ASTER_APP_PRIVATE_KEY
```

The chart injects required Secret references into both the server and worker, or the fetcher CronJob in cron mode, independently of `ai.enabled`. It does not create the App Secret. Keep the GitHub App Secret separate from the model-provider Secret. Remove old conflicting App entries from `fetcher.extraEnv` and `server.extraEnv`; explicit entries there are rendered last. Existing Secret-backed App entries remain supported.

For direct CLI or non-Helm deployments, supply both environment variables to each process that performs GitHub reads. Never put the private key in `project.yaml`, prompts, or checked-in Helm values.

`aster onboard doctor` checks the effective App credential references for the fetcher and server, including incomplete pairs and optional or blank overrides. Kubernetes doctor also checks that the referenced Secret metadata exists. It intentionally does not read Secret keys or values, test installations, or verify GitHub permissions. Validate those prerequisites through the App and Secret-management workflows.

## Credential precedence and failure behavior

When either App variable is nonempty, Aster selects App authentication. An incomplete pair or invalid private key fails configuration. A missing installation, denied permission, suspended installation, or failed token renewal fails the affected read without trying another credential.

When no App is configured, the existing modes remain available:

| Read path | Static credential order |
| --- | --- |
| Worker, one-shot fetch, and job discovery | `GITHUB_READ_TOKEN`, then `GITHUB_TOKEN`, then anonymous. |
| Server chat and escalation | `GITHUB_READ_TOKEN`, then `BOT_TOKEN`, then `GITHUB_TOKEN`, then anonymous. |
| Fix source checks | `SOURCE_INVESTIGATION_GITHUB_TOKEN`, then the server order above. |

App authentication precedes all of these static choices. The source-check override does not override an App. The existing `BOT_TOKEN` and comment authentication still control their respective writes.

Tokens are obtained before each GitHub read and cached by repository and read permission until five minutes before expiry. Concurrent requests share a renewal. A renewal failure returns an error instead of using an expiring cached token. Tokens stay in memory and are never logged, published, or persisted in analysis state. Normal publication behavior still applies: for example, failed triage retains its previous view.

## Private-key rotation

GitHub installation tokens expire after one hour; Aster renews them automatically using the App private key. This does not require changing a Secret or restarting pods. See [GitHub installation authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation).

The App private key is a separate, longer-lived credential that operators rotate occasionally. Generate a replacement key, update the Secret through its owner, and restart both worker and server so their environment variables load it. After the rollout succeeds, revoke the old key. Updating an environment-backed Secret alone does not refresh running processes.
