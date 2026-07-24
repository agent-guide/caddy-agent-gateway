# Plans

This section holds time-bound execution plans, as opposed to the durable
design and decision documents in `design/`. Each plan targets a specific
version line or implementation effort, is verified against a specific tree
snapshot, and becomes obsolete by design once its work lands.

Lifecycle convention:

- a plan states the version line or effort it targets
- when the code and the plan disagree, the code wins and the plan is wrong
- when every item in a plan is either completed or explicitly re-homed, and
  the permanent documents (`design/`, `architecture/`, `README.md`,
  `AGENTS.md`) describe the resulting behavior, the plan is deleted — git
  history is the archive

Current plans:

- [v0.4-completion.md](v0.4-completion.md): remaining work to close the
  `v0.4.x` line
- [observability-implementation.md](observability-implementation.md):
  implementation plan companion to `design/observability.md`
- [unified-agent-runtime.md](unified-agent-runtime.md): implementation plan for
  one turn-first Agent runtime capability layer and one AgentRoute across ACP,
  HTTP, and builtin Agent identities, with ACP config owned directly by
  `Agent.runtime.acp` rather than a separate service object; durable task/DAG
  execution remains owned by the
  [Unified Workflow Runtime](../design/workflow-runtime.md)
