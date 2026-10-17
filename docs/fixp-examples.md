# FIXP's examples, and the tests running them

Every usage example of FIXP 1.0's section 7 (Appendix A, TCP), with what
fix2's engine does and the tests that check it: as the example shows
("as shown"), or as [docs/fixp.md](fixp.md#where-the-standard-contradicts-itself)
records it diverging ("diverges"), or out of scope ("excluded"). Tests are
in `fixp/session`; those named `TestExample_…` are in
[`examples_test.go`](../fixp/session/examples_test.go), written for the
examples the other tests did not run. The examples' values are
illustrative (§7): the tests check the exchange and its codes, reasons
where the example gives one fix2 shares, not the sample numbers.

## 7.1 Initialization

| Example | Shows | fix2 | Tests |
|---|---|---|---|
| 7.1.1 | Negotiate, both flows Recoverable | as shown | `TestExample_7_1_and_7_2_Handshakes/7.1.1`, `TestFinalize` |
| 7.1.2 | both Unsequenced | as shown | `TestExample_7_1_and_7_2_Handshakes/7.1.2`, `TestUnsequenced` |
| 7.1.3 | client Idempotent, server Recoverable | as shown | `TestExample_7_1_and_7_2_Handshakes/7.1.3`, `TestSequenced` |
| 7.1.4 | titled client None, server Recoverable; shows server Unsequenced | both pairs work (row "Example §7.1.4") | `TestExample_7_1_4_NoneRecoverable`, `TestExample_7_1_and_7_2_Handshakes/7.1.4…`, `TestFinalize/None/Unsequenced` |
| 7.1.5 | client Unsequenced, server Recoverable | as shown | `TestExample_7_1_and_7_2_Handshakes/7.1.5`, `TestRetransmitServed` |
| 7.1.6 | client None, server Unsequenced | as shown | `TestExample_7_1_and_7_2_Handshakes/7.1.4 as shown, 7.1.6`, `TestSendErrors` |
| 7.1.7.1 | bad credentials: `Credentials` | as shown | `TestRejections/credentials` |
| 7.1.7.2 | flow not supported: `FlowTypeNotSupported` | as shown | `TestRejections/client flow` |
| 7.1.7.3 | invalid session ID: `Unspecified` | as shown | `TestNegotiateChecks` |
| 7.1.7.4 | timestamp in seconds: `Unspecified` | as shown | `TestExample_7_1_7_4_TimestampInSeconds`, `TestNegotiateChecks` |
| 7.1.7.5 | response of another session ignored; negotiates again, new ID | as shown | `TestExample_7_1_7_5_ResponseOfAnotherSession`, `TestClientNegotiationRetries` |
| 7.1.7.6 | no response: negotiates again, new ID | as shown | `TestClientNegotiationRetries` |

## 7.2 Binding

| Example | Shows | fix2 | Tests |
|---|---|---|---|
| 7.2.1 | Establish and ack, Recoverable, `NextSeqNo` 1 both ways | as shown | `TestExample_7_1_and_7_2_Handshakes/7.1.1, 7.2.1` |
| 7.2.2 | Unsequenced, no `NextSeqNo` | as shown | `TestExample_7_1_and_7_2_Handshakes/7.1.2, 7.2.2` |
| 7.2.3 | Idempotent client, Recoverable server, `NextSeqNo` both ways | as shown (row "Establish `NextSeqNo`") | `TestExample_7_1_and_7_2_Handshakes/7.1.3, 7.2.3`, `TestSpikeReplay` |
| 7.2.4 | a None/None session | diverges: refused at Negotiate (row "A None flow both ways") | `TestRejections/None both ways` |
| 7.2.5.1 | Establish unnegotiated: `Unnegotiated` | as shown | `TestDuplicateAndAlreadyEstablished`, `TestRetire` |
| 7.2.5.2 | Establish again: `AlreadyEstablished` | as shown, on the same connection or another | `TestExample_7_2_5_2_AlreadyEstablishedHere`, `TestDuplicateAndAlreadyEstablished` |
| 7.2.5.3 | blocked session: `SessionBlocked` | as shown, through the `Authenticate` hook; a client keeps trying | `TestExample_7_2_5_3_SessionBlocked` |
| 7.2.5.4 | keepalive out of range: `KeepaliveInterval` | as shown | `TestRejections/keepalive interval` |
| 7.2.5.5 | invalid session ID: `Unspecified` | as shown | `TestExample_7_2_5_5_InvalidSessionID` |
| 7.2.5.6 | timestamp in seconds: `Unspecified` | as shown | `TestExample_7_2_5_6_TimestampInSeconds` |
| 7.2.5.7 | bad credentials: `Credentials` | as shown | `TestOwnerBinding` |
| 7.2.5.8 | ack of another request ignored; establishes again, same ID | as shown | `TestExample_7_2_5_8_and_9_EstablishUnanswered` |
| 7.2.5.9 | no ack: establishes again, same ID | as shown | `TestExample_7_2_5_8_and_9_EstablishUnanswered` |

## 7.3 Unbinding

| Example | Shows | fix2 | Tests |
|---|---|---|---|
| 7.3.1 | keepalive lapsed: Terminate, establish again | diverges: `UnspecifiedError`, a new connection (rows "Termination code on a lapsed keepalive", "Establishing again after Terminate") | `TestExample_7_3_1_and_7_3_2_EstablishAgain/7.3.1`, `TestKeepaliveLapsed` |
| 7.3.2 | Sequence going back: Terminate, establish again | as shown, but establishes again on a new connection (row "Establishing again after Terminate") | `TestExample_7_3_1_and_7_3_2_EstablishAgain/7.3.2`, `TestProtocolViolations/Sequence going back` |
| 7.3.3 | ack's `NextSeqNo` going back: the client terminates | as shown, on a new connection; a server rejects the same `Unspecified` | `TestExample_7_3_3_NextSeqNoGoingBack` |
| 7.3.4 | Terminate `Finished` answered; established again, same ID | as shown | `TestTerminateReestablish` |
| 7.3.5 | disconnection: negotiates a new ID | diverges: re-establishes under the same ID (row "Reconnecting after a disconnection") | `TestSendWhileUnbound`, `TestClientResumes` |

## 7.4 Transferring

| Example | Shows | fix2 | Tests |
|---|---|---|---|
| 7.4.1 | Sequence, then implicit numbering; the ack numbers the server's first message | as shown | `TestExample_7_4_1_NumberedByTheAck`, `TestObserver`, `TestRecoverableGapFilled` |
| 7.4.1.1 | Sequence going up: NotApplied | as shown, its arithmetic corrected (row "Example §7.4.1.1") | `TestIdempotentGap`, `TestCrashIdempotentGapAtEveryWrite` |
| 7.4.1.2 | Sequence going back: Terminate | as shown | `TestProtocolViolations/Sequence going back` |
| 7.4.1.3 | Sequence as heartbeat, both ways | as shown | `TestExample_7_4_1_3_and_7_4_3_Heartbeats/7.4.1.3`, `TestKeepalive` |
| 7.4.2, 7.4.2.1 | Context, multiplexing | excluded: one session per connection | `TestProtocolViolations` (Topic, Context refused) |
| 7.4.3 | UnsequencedHeartbeat | as shown | `TestExample_7_4_1_3_and_7_4_3_Heartbeats/7.4.3`, `TestKeepalive` |
| 7.4.4 | RetransmitRequest, Retransmission; later messages held | as shown | `TestRetransmitServed`, `TestRecoverableGapFilled` |
| 7.4.4.1 | a request while one is outstanding: `ReRequestInProgress` | as shown | `TestReRequestInProgress`, `TestRetransmitOwnership` |
| 7.4.4.2 | batches interleaved; a second batch for one request | diverges: one batch per request, the rest asked for (row "Batches answering one RetransmitRequest") | `TestExample_7_4_4_2_PartialBatch`, `TestRetransmitServed` |
| 7.4.5.1 | `FromSeqNo` beyond what was sent: `OutOfRange` | as shown (row "RetransmitRequest beyond what was sent") | `TestRetransmitServed` |
| 7.4.5.2 | a range partly beyond: `OutOfRange` | as shown, refused whole | `TestExample_7_4_5_2_PartlyBeyond` |
| 7.4.5.3 | another session: `InvalidSession` | as shown | `TestRetransmitServed` |
| 7.4.5.4 | too many asked: `RequestLimitExceeded` | as receiver, asks for less; as sender, answers a partial batch instead | `TestRecoverableRequestLimit`, `TestRetransmitServed` |
| 7.4.5.5 | messages no longer kept: `ReRequestOutOfBounds` | diverges: `OutOfRange`, "no longer available" (row "Messages no longer kept") | `TestRetransmitServed`, `TestRetention` |

## 7.5 Finalizing

| Example | Shows | fix2 | Tests |
|---|---|---|---|
| 7.5.1 | FinishedSending and FinishedReceiving both ways, then Terminate | as shown, its own FinishedSending before FinishedReceiving (row "A peer's FinishedSending answered") | `TestFinalize`, `TestFinishRecoverableCompletes` |
| 7.5.2 | FinishedSending repeated until answered | as shown, and until finalized (§4.7.1) | `TestExample_7_5_2_FinishedSendingRepeated`, `TestFinishedSendingRepeatedCoalesced`, `TestFinishResumedOnReconnect` |
| 7.5.3 | FinishedSending past a gap: RetransmitRequest, then FinishedReceiving | as shown; NotApplied on an Idempotent flow | `TestFinishRecoverableCompletes`, `TestFinishIdempotentGap` |
| 7.5.4 | Terminate before both flows finished: a violation | diverges: answered, the session re-establishable (rows "When the session is finalized", "Answer to a Terminate before both flows are finished") | `TestExample_7_5_4_TerminateBeforeFinished` |
| 7.5.5 | Sequence after FinishedSending: Terminate | as shown | `TestExample_7_5_5_SequenceAfterFinishedSending`, `TestFinishViolations`, `TestBeyondLastSeqNo` |
| 7.5.6 | half close | diverges on what follows FinishedReceiving and a RetransmitRequest after one's own FinishedSending (rows of those names) | `TestExample_7_5_6_HalfClose` |
