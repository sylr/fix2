# Sixth audit: session protocol and closing review

Audited revision: `7a6734b70c468a797d01cfaf0366c86c5f9fbe81`. Read audit five and its addendum, reviewed production diff since `0da2580`, and inspected surrounding gap, stash, reset, final-close, queue-admission and replay retirement paths. Production source and existing tests were not edited. New regression source ran in `/private/tmp/fix2-audit6-session.ZpfMU5`, an isolated archive of HEAD.

## Confirmed P3: direct Logout releases later held application messages

**Confidence:** high for the reproducible asymmetry. Narrow protocol-completion consistency defect; not a default P2 production blocker.

`session/protocol.go:1209–1215` handles a peer Logout received in sequence by marking it and calling `advanceLocked` **before** setting the Logout phase and entering final closure. The phase guard in `advanceLocked:790–792` therefore still sees the established phase and drains buffered messages with higher sequence numbers, delivering application orders after the peer's final sequence-position Logout. Once those orders have drained, the engine sends its Logout response and closes.

The same logical sequence boundary is enforced when the Logout itself was previously held: `replayStashedLocked` invokes `onLogoutLocked` while a drain is already running; the nested `advanceLocked` only changes the target because `s.draining` is true (`protocol.go:781–783`). The Logout phase then changes before the outer drain continues, so its guard leaves subsequent held applications untouched. Existing `TestHeldArrival_HeldLogoutStopsHeld` explicitly asserts this policy. The direct in-sequence case misses that boundary.

### Reproduction and controls

`in-sequence-logout-drain_test.go.txt` contains three cases using public session APIs and existing scripted peer helpers, all under `CompatStrict2020`:

1. **Heartbeat control:** receive order sequence 3 while expecting 2; wait for the ResendRequest; receive Heartbeat 2. Order 3 is delivered normally, target becomes 4. Passes.
2. **Direct Logout:** same held order 3, but fill sequence 2 with Logout. Order 3 is still delivered before disconnect, target becomes 4. Fails the final sequence-boundary assertion.
3. **Held Logout control:** receive Logout 3 while expecting 2; wait for the ResendRequest; receive order 4, then Heartbeat 2. No order is delivered, and target stops at 4, preserving the existing held-Logout behavior. Passes.

All three race-enabled executions have this exact result; no data race was reported. See `in-sequence-logout-drain.log`. There are no sleep-based correctness assumptions: ResendRequest synchronizes stashing, and OnLogout synchronizes completion of earlier deliveries.

### Impact and exposure boundaries

The application can observe a buffered order beyond the peer's final Logout in the direct case while the logically equivalent held-Logout case preserves it for later recovery. A counterparty must provide an ahead-of-sequence application message and a lower-sequence Logout; ordinary correctly sequenced traffic does not trigger this. No application-order loss, duplicate execution, authentication bypass or privilege escalation was demonstrated. This is **not** a blanket claim that callbacks while logging out are prohibited: ordinary traffic during the earlier grace period while awaiting a peer's answer is intentionally handled.

Suggested correction: establish one final peer-Logout sequence boundary before releasing any later held messages, consistently for direct and stashed Logout handling. Preserve the intentional earlier Logout grace period, recovery of messages before Logout, queue/receipt accounting, and LastMsgSeqNumProcessed. Regress direct and held variants together.

## A5-01 and A5-02 remediation

Targeted existing tests pass under race, including:

- blocked replay final-close deadline for peer-initiated and crossing Logout;
- final Logout as last frame for TestRequest, Reject and ResendRequest input;
- BusinessMessageReject suppression without incrementing RejectsSent;
- a resend asked for before Logout completing before the answer;
- stalled-handler closure ignoring reset Logon;
- slow healthy Logout writes, including writes exceeding LogoutTimeout;
- original arrival time, held GapFill/Logout and fatal-transition drain regressions inherited from A4-01.

Command:

```sh
GOCACHE=/private/tmp/fix2-production-readiness-4-gocache go test -race -count=1 \
  -run '^(TestLogoutAnswer|TestLogoutAnswered|TestReplay.*Logout|TestStallLogoutIgnoresResetLogon|TestHeldArrival_|TestHeldLogoutStopsDrain)' \
  -v ./session
```

Result: passed in 10.389 seconds. Full selected output: `a5-remediation.log`.

Source inspection confirms the replay-only close deadline now arms at final closure, expiry disconnects and cancels replay, new output uses no sequence number when closing, replay admission refuses closing connections, and reset Logon cannot escape final closure. No recurrence of the specific A5-01 or A5-02 demonstrations was established. Broader storage, transport, receipt-history and deployment boundaries are covered by the leader and independent runtime review.
