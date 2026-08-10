# Optional features

Deploy the basic dashboard first. Confirm the expected jobs and analyses, then
add one optional feature at a time.

Authenticated chat, File Issue, and Mark Resolved use the standard Kubernetes
server and in-process analysis. They do not require Orka or a Fix PR coding-agent
runtime.

## Feature overview

| Feature | Required deployment | Risk and effect | Reference |
| --- | --- | --- | --- |
| Analysis chat | Kubernetes with authentication | Read-only model conversations stored as private server state | [Analysis chat API](server.md#analysis-chat-api) |
| File Issue | Kubernetes with authenticated actions | Creates a reviewed GitHub issue after explicit confirmation | [Admin-gated actions](server.md#admin-gated-actions) |
| Mark Resolved | Kubernetes with authenticated actions | Writes dashboard resolution state, but does not change source code | [Admin-gated actions](server.md#admin-gated-actions) |
| Email notifications | Pages or Kubernetes | Sends failure summaries to configured recipients | [Email notifications](notifications.md) |
| Automatic GitHub issues | Pages or Kubernetes | Creates and updates GitHub issues during fetches | [GitHub issues](github-issues.md) |
| Fix PR generation | Custom Pages runner, local sandbox, Orka, or consumer-installed Agent Sandbox | Experimental, highest-risk code-writing automation | [Experimental Fix PR generation](fix-prs.md) |
| Source investigation | Kubernetes plus a separate Orka evaluation deployment | Experimental read-only external Agent workflow | [Source investigation](server.md#source-investigation-api) |
| Independent causal critic | Kubernetes plus consumer-installed Agent Sandbox and internal model gateway | Private sampled review only; never changes publication or writes | [Agent Sandbox causal critic](agent-sandbox-causal-critic.md) |

Static Pages sites do not serve authenticated interactive APIs. They can run
scheduled notifications and issue automation during the fetch workflow when the
required credentials are configured.

## Recommended order

1. Deploy the read-only dashboard with in-process analysis.
2. Add authentication and analysis chat if maintainers need private follow-up
   conversations.
3. Enable File Issue and Mark Resolved after the authentication boundary is
   reviewed.
4. Add notifications or scheduled issue automation with narrowly scoped
   credentials.
5. Evaluate Fix PR generation or external runtimes only in a separate,
   explicitly experimental rollout.

The focused references contain the complete credential, authorization,
retention, and operational requirements. Do not copy their advanced examples
into the first-run configuration unless the feature is being enabled.
