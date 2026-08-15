# Example project (E2E fixture)

This is a synthetic prompt addendum used by the engine's end-to-end pipeline
tests. The project under test provisions example workload clusters and runs E2E
suites against them.

## Common failure modes

- Control-plane provisioning timeouts when machines fail to register.
- Cleanup failures leaving leaked infrastructure.
