# Core directive review for proactivity

Reviewed on 2026-09-09. Core source was read only; no core files were changed. The server capability evaluation reads the existing main and worker prompt constants directly and records a hash of the source files.

## What may interfere

- `core/thread.go:33` tells a leaf worker, “If you have no events to process, just sleep.” Its continuing-work instruction at line 42 tells it to own its domain work and cadence. The first sentence can compete with autonomous initiative on a timer-only wake. This is a wording conflict, not a hard runtime gate. The Codex capability evaluation includes this exact template so its effect can be observed.
- `core/thread.go:71` tells a leader, “Only spawn threads that are defined in your team. Do not invent new thread IDs.” That may prevent new ownership for newly discovered opportunities in a leader's domain. The current capability comparison covers the main and leaf-worker templates, not this leader template; this remains a potential restriction from inspection.
- `core/thinker.go:235` keeps main's direct work very small and routes substantial work through focused owners. A benchmark that offers only direct actions can understate initiative or force a prompt conflict. The expanded evaluation supplies a spawn capability to main and the policy-only context. Leaf workers correctly do not get spawn.
- `core/thinker.go:3212` waits for an agent-owned pending wake, or for an event when there is no pending wake. `core/pace.go:126` consumes a fired timer unless the model has replaced it. There is no separate recurring scheduler that forces continued proactive review. If the agent does not rearm a purposeful wake, it can stay idle regardless of proactivity. The new setting is a prompt policy; this evaluation does not establish reliable ongoing scheduling.
- `core/thinker.go:418` instructs the first thought to act on its mission, overriding default idle behavior. This may compete with a reactive setting when the mission is only a broad goal. The server explicitly says startup is not itself a new assignment, and the capability test includes the real startup wrapper in its main prompt variant.
- `core/prompt_contract.go:7` asks the agent to retain reusable method improvements after completing work. It does not qualify this by proactivity. At zero, this can create ambiguity between routine retention of an assigned task's learning and initiating a new improvement. It was not tested as a separate evolve-tool scenario here.

## Scope of the evaluation

The earlier five-tool matrix contained no core prompt. Its failure to separate the higher levels therefore could not have been caused by core. The broader comparison adds actual core main/leaf-worker constants and richer simulated tools to isolate prompt interactions. It does not import or execute core, instantiate real workers, or test repeated wake cycles. Dynamic provider catalogs, full tool documentation, recalled memories, and real runtime history are omitted.

A change to core would require a separate implementation decision. Findings here distinguish potential prompt conflicts from observed test failures; they are not a claim that a specific line universally blocks proactivity.


## Observed effect after sharpening

The tightened server policy makes the existing-lead requirement explicit through 75%. The main-prompt comparison distinguishes inspection of observed problems (50+), validation of uncertain leads (75+), and discovery in unexamined areas (100). Including the core main and worker templates did not establish a general blocker to initiative in the capability tests. In particular, discovery at 100 was selected with the worker's no-events sleep sentence still present. This narrows the finding to potential friction in other situations, not a reason to change core based on this test alone. See the dated sharpening report for final counts and limitations.
