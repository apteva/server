# Hosted app process supervision

The stable host owns and reaps sidecars using `exec.Cmd.Wait`. Management-slot
supervision subscribes to process-exit notifications without trying to reap
another process's child: pidfd + poll on Linux, EVFILT_PROC/NOTE_EXIT on macOS.
There is no 100 ms process-helper loop.

Identity checks read kernel executable metadata and a process-birth token. They
do not trust argv and never execute ps. Linux also compares the executable inode.
A subscription is bracketed by identity checks to detect PID reuse. Managed
shell-to-executable startup has a bounded two-second allowance before tracking.

Exit notifications wake immediately. A 30-second native identity audit detects
unexpected executable changes. If the kernel/container cannot provide native
notifications, one-second kernel identity checks provide bounded fallback failure
detection. Permission/transient failures remain unknown rather than confirming
termination. Captured hosted handles recheck their birth/executable before signals.
An old process exit removes only that same handle, preserving a replacement.

This changes process supervision only: app health, admission, deployment ownership,
managed restart and stable-host process ownership remain separate.

Validation uses disposable local helper processes and network-disabled Linux
containers, including reattachment, duplicate tracking, process replacement,
confirmed stop/StopAll, wrong-identity signal refusal and fallback detection.
Benchmarks include parent and short-lived child CPU so helper costs are visible.
No running installation is changed by these source edits.
