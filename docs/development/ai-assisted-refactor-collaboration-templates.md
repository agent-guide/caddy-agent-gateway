# AI-Assisted Refactor Collaboration Templates

## Purpose

This document provides prompt templates and a lightweight workflow for using a large model to drive architecture refactors with lower human involvement while preserving codebase maturity and long-term maintainability.

The goal is not to ask the model to "just refactor", but to constrain it with explicit architectural boundaries, small execution steps, and repeatable audit checkpoints.

## Core Principle

Do not ask the model to do design, implementation, and review in one pass.

Split work into four stages:

1. architecture diagnosis
2. refactor design
3. single-step implementation
4. single-step audit

Each stage should have:

- a narrow objective
- a fixed output format
- clear file scope
- explicit forbidden actions

## Recommended Workflow

Use this sequence for each refactor cycle:

1. run the Architecture Diagnosis prompt
2. run the Refactor Design prompt
3. confirm the step list, not the entire implementation details
4. run only one Single-Step Implementation prompt at a time
5. after each step, run the Single-Step Review / Audit prompt
6. if problems are found, fix the current step before moving on
7. after a batch of steps, run the Phase Acceptance prompt

## Persistent Constraints

These negative constraints are worth repeating in nearly every prompt:

```text
Negative constraints:
- Do not unify things just for the sake of unification.
- Do not move domain runtime logic into shared core packages.
- Do not create types with generic names but domain-specific responsibilities.
- Do not change ingress, runtime, and persistence in the same step unless explicitly required.
- Do not combine renaming, moving code, abstraction upgrades, and behavior changes in one step.
- Do not keep a transitional double-model design for more than one step.
- Do not reduce file count at the expense of boundary clarity.
```

## Template 1: Architecture Diagnosis

Use this before any code changes.

```text
You are acting as a senior software architect. Do not write code. Only perform architecture diagnosis.

Repository background:
[Paste the relevant AGENTS.md / README / design notes here]

Current refactor goal:
[Example: unify the shared route matching capability between LLM routes and MCP routes, but do not move LLM domain logic into route core]

Please do only the following:

1. Identify the main responsibility overlaps, duplication points, and cross-layer dependencies in the current code.
2. Separate current code responsibilities into:
   - shared infrastructure
   - route config layer
   - runtime resolution layer
   - protocol ingress layer
   - persistence layer
3. Explicitly call out which responsibilities must not be moved into a shared layer.
4. Define the boundary of this refactor:
   - what should be unified
   - what should not be unified
5. Produce 3-6 architecture constraints that all later code changes must follow.

Restrictions:
- Do not write code.
- Do not give a giant end-state redesign.
- Do not discuss optional future expansion. Only discuss the minimum boundaries needed for this refactor.
- If a proposed unification would make a shared package depend on domain logic, explicitly reject it.

Required output format:

A. Current Problems
B. Suggested Boundaries To Preserve
C. Responsibilities That Must Not Move Into Shared Layers
D. Refactor Constraints
E. Candidate Small Steps (titles only)
```

## Template 2: Refactor Design

Use this after diagnosis and before implementation.

```text
Continue from the architecture constraints above. Do not write code.

Goal:
[State the exact goal for this refactor round]

Break the refactor into 5-10 independently shippable small steps.

For each step, include:
1. step name
2. objective
3. allowed files/directories
4. forbidden files/directories
5. expected output
6. completion criteria
7. main risks
8. whether rollback is easy

Hard constraints:
- Each step must solve only one architecture problem.
- Do not "clean up extra things on the way".
- Do not introduce provider/modelcatalog/credentialmgr or other domain dependencies into core packages.
- If a step crosses too many layers, split it further.
- Prefer boundary clarification first, code convergence second.

Output format:
Use a table or numbered list. Do not write code.
```

## Template 3: Single-Step Implementation

Use this to execute exactly one step.

```text
Execute only step [N] from the approved refactor plan. Do not do step [N+1].

Step objective:
[Paste the step objective]

Allowed modification scope:
[List files/directories]

Forbidden scope:
[List files/directories]

Architecture constraints that must be followed:
[Paste the approved constraints]

Implementation requirements:
1. First explain in 5-10 sentences how you intend to change the code.
2. Explicitly state what this step will not solve.
3. Only then perform the code changes.
4. After the change, perform self-checks for:
   - new cross-layer dependencies
   - domain logic moved into shared layers
   - duplicate models introduced
   - ingress behavior accidentally changed
5. If you discover this step cannot be done without breaking scope, stop and explain why. Do not silently expand the scope.

Required output format:

A. Implementation Approach
B. Actual Changes
C. Intentionally Unchanged
D. Self-Check Results
E. Risks
```

## Template 4: Single-Step Review / Audit

Use this after every implementation step.

```text
You are now an architecture auditor, not the implementer. Do not give generic advice. Identify concrete issues.

Audit the just-completed refactor step and focus on:

1. whether any new cross-layer dependency was introduced
2. whether logic that belongs to an LLM or MCP subdomain was moved into a shared layer
3. whether configuration was unified but runtime boundaries became more confused
4. whether unification introduced double abstractions
5. whether naming became misleading, making domain code look generic
6. whether this step creates design debt that will make later steps harder

Required output format:

1. Findings
- severe issues
- medium issues
- minor issues

2. Boundary Check
- boundaries preserved
- boundaries violated

3. Over-Abstraction Check
- abstractions that are justified
- abstractions that are excessive

4. Next-Step Readiness
- whether the next step should proceed
- if not, what must be fixed first

Requirements:
- conclusions must be specific to packages, files, types, and function responsibilities
- do not praise the implementation
- findings come first
```

## Template 5: Phase Acceptance

Use this after a batch of steps to verify the refactor is actually improving structure.

```text
Perform a phase-level architecture acceptance review. Do not write code.

Acceptance criteria:
1. shared layers only contain cross-domain common capabilities
2. LLM and MCP each still retain their own runtime interpretation authority
3. ingress unification does not pollute gateway core
4. persistence changes remain consistent with runtime boundaries
5. coupling was reduced rather than just moved upward
6. adding a new protocol later would now be easier

Required output format:

A. Current Layer Diagram (text only)
B. Responsibility Of Each Layer
C. What Each Layer Must Not Do
D. What This Refactor Actually Improved
E. Structural Risks Still Remaining
F. Whether Another Refactor Round Is Recommended
```

## Required Audit Card

Ask the model to append this after every implementation step:

```text
Please append a change audit card:

- New dependencies introduced:
- Dependencies removed:
- Responsibilities converged in this step:
- Couplings still remaining:
- New shared abstractions introduced:
- Whether each shared abstraction is truly cross-domain:
- Safest next step:
```

## Low-Human-Involvement Mode

If the goal is to reduce human review time without losing too much control, separate the model's work into three roles even if the same underlying model is used:

- Architect: only defines boundaries and step plans
- Implementer: only executes the current approved step
- Reviewer: only finds structural problems

Do not ask one prompt to both "boldly improve the architecture" and "finish the whole refactor". That strongly increases the chance of over-unified, overly abstract designs.

## Suggested Usage Notes

- Keep each refactor step small enough that a human can understand its intent in a few minutes.
- Review against a fixed checklist rather than rereading the whole diff from scratch.
- Prefer package-level boundary cleanup before type-level abstraction cleanup.
- If the model starts proposing a "grand unified core", force it to justify dependency direction explicitly.
- Treat "what must not move" as equally important as "what should be shared".
