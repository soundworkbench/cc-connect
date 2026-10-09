# Codex background completion wakeup

Opt-in for the `app_server` backend; disabled by default. Put these in the
project's `[projects.agent.options]`:

```toml
background_wakeup = true
background_wakeup_max_turns = 10
background_wakeup_timeout_mins = 60
```

The adapter accepts only parent-scoped `subAgentActivity` items with kind
`completed`, including those arriving after their originating turn. Child
turn/output isolation is unchanged. Events are deduplicated and coalesced;
the engine reserves the normal session execution slot before an automatic
turn, so foreground input queues/steers rather than starting a competing turn.
The parent reads native results and continues only existing authorized work.

A real user input arms a bounded lease. Automatic input does not renew it.
Explicit stop/cancel/pause text disables wakeups conservatively; `/stop`, close,
restart, parent failure, known exhausted quota, deadline, or the configured turn
budget prevent further automatic starts. Transport failures are not retried.
The string stop guard is conservative (even quoted stop words can disable the
lease). Later non-stop user input can arm a new lease.

This is not a durable job scheduler: process exit does not reconstruct tasks,
missed events are not polled from history, and idle process closure still ends
the lease. No cron is installed. Deadlines bound new starts, not an already
running turn; `/stop` remains the immediate cancellation control. Model/tool
permissions are unchanged. Monetary budget enforcement is not implemented;
turn limits and reported quota are the available safeguards.

Validation: deterministic scope/dedup/busy/stop/expiry/budget tests, a core CUJ
for two unsolicited batches plus interleaved user input and `/stop`, and the
explicit `CC_CONNECT_WAKEUP_LIVE=1` two-batch test. Production rollout requires
the full test suite and a real messaging check; a unit-test pass alone is
not a production completion claim.
