# Conformance

Which standards fix2 follows, in which editions, and every place where
it departs from them, on purpose and behind an option, or where they
leave the choice open or contradict themselves.

## The rule

fix2 follows the FIX, FIXP, SOFH and SBE specifications strictly by
default. A deviation that venues or other implementations need is an
opt-in configuration named `Tolerate<Behaviour>`, off by default; one
that changes what fix2 sends is named after the peer behaviour it
accommodates. An option for a choice a specification leaves to the
implementation takes no prefix. Where a specification contradicts
itself, this document records fix2's reading, which is not a
toleration. Venue protocols that are not FIX Trading Community
standards, such as Binance's "FIX SBE", are out of scope.

"Strictly" means the editions and addenda pinned below, and their
normative text: what they require ("must", "shall", "required") is the
default; what they recommend or allow ("should", "may") is not a
toleration when fix2 does otherwise, and takes no prefix.

How to read the rest:

- [Pinned editions](#pinned-editions) lists the texts every reference
  below is to, with their hashes.
- [Tolerations](#tolerations) lists every `Tolerate` option, what it
  accepts, the text it departs from, and whether
  `session.QuickFIXTolerations()` enables it.
- [Strictness without option](#strictness-without-option) lists what
  fix2 refuses, or accepts, to conform, with no way back.
- [Implementation choices](#implementation-choices) lists the options
  without prefix and the text that leaves them open.
- [Contradictions](#contradictions) gives fix2's reading where the texts
  disagree.

Where a pinned text says "should", fix2 does what it says by default,
and an option for doing otherwise takes no prefix: it is listed under
[Implementation choices](#implementation-choices). The FIXT test cases
illustrate the texts; their "Mandatory" marks do not turn a "should"
into a requirement.

Most rows are planned: the Status column names the milestone of the
strict-by-default effort that delivers them (see
[Milestones](#milestones)). Since S1 the FIX engine is strict by
default: `Compatibility`, whose default `CompatQuickFIX` departed from
the texts where QuickFIX does, is gone.

Quotes are exact, from the text layers of the files hashed below, the
spacing the FIXT 1.1 PDF's text layer inserts inside words removed.
"(to confirm)" marks a row whose classification the text does not
plainly support; "(unverified)" a reference not checked against a
pinned text.

## Pinned editions

| Abbr. | Standard | Edition | Source | SHA-256 |
|---|---|---|---|---|
| — | FIX 4.0, 4.1, 4.2, 4.3, 4.4 application and session message definitions | No base specification text is pinned: fix2 validates against the dictionary the application supplies, a QuickFIX XML dictionary or a FIX Orchestra repository. Its tests use trimmed copies in [`fix/dictionary/testdata`](../fix/dictionary/testdata): `FIX40.xml`, `FIX44.xml`, `FIX50SP2.xml` derived from [quickfixgo/quickfix](https://github.com/quickfixgo/quickfix) at `eff945dd59a41caeaed20f71916f08d37e0fda99`, `OrchestraFIXLatest.xml` from [FIXTradingCommunity/orchestrations](https://github.com/FIXTradingCommunity/orchestrations) (FIX Latest, EP312) | see the files' headers | not pinned |
| SL | FIX Session Layer Technical Specification | Technical Standard, June 2020 | [FIX_Session_Layer_June_2020.pdf](https://www.fixtrading.org/wp-content/uploads/download-manager-files/FIX_Session_Layer_June_2020.pdf) | `661c6bd90db9d67a050f0cd58230896d5d22f9448a942a8453d3595d0455f655` |
| TV | FIX TagValue Encoding Technical Specification, a normative reference of SL (§2) | Version 1.0, Technical Standard, June 2020 | [fixtrading.org/packages/fix-tagvalue-encoding](https://fixtrading.org/packages/fix-tagvalue-encoding/) | `8d9793e05258c45950a35be458017ed6c02bdf0fb08d348848f335c1a27d2083` |
| FIXT | FIX Session Protocol, FIXT.1.1 | Version 1.1 Errata, March 2008, with its "FIX Session-level Test Cases and Expected Behaviors" | [FIX_Transport_1.1.pdf](https://www.fixtrading.org/wp-content/uploads/download-manager-files/FIX_Transport_1.1.pdf) | `407e5438bc71056ba43ef8ca456a08ff6facfc396c1573b964535a6e624603a5` |
| TPA | FIX Technical Addendum, Time Precision Enhancement | Revision 1, approved, effective September 12, 2016. It replaces the UTCTimestamp and UTCTimeOnly definitions of FIX 4.2 and FIX 4.4, and those and TZTimestamp, TZTimeOnly of FIX 5.0, 5.0 SP1 and 5.0 SP2 (its Table 1). It does not amend FIX 4.0, 4.1 or 4.3. | [FIX-Technical-Addendum-Time-Precision.pdf](https://www.fixtrading.org/wp-content/uploads/download-manager-files/FIX-Technical-Addendum-Time-Precision.pdf) | `da919a352db9476d056683a1f3d646f7fc0bc1cf6548578d6fe3ad3c9e61138c` |
| FIXP | FIX Performance Session Layer | Version 1.0, Technical Standard, April 15, 2021; session schema pinned in [docs/fixp.md](fixp.md#the-schema) | [FIX_Performance_Session_Layer_V1.0.pdf](https://www.fixtrading.org/wp-content/uploads/download-manager-files/FIX_Performance_Session_Layer_V1.0.pdf) | `13a123328f2f0bfb94ce2626961203b0f7ac27e2c56eca28d8b8230958201567` |
| SOFH | Simple Open Framing Header | Version 1.0, Draft Standard, September 6, 2016: version 1.0 is published as a Draft Standard only | [fixtrading.org/packages/fix-simple-open-framing-header-draft-standard-v1-0](https://fixtrading.org/packages/fix-simple-open-framing-header-draft-standard-v1-0/) | `9dcf8d7702a07d3308282e6c73225a72260c32de64f0fbb5cc59facc03ca45ea` |
| SBE | Simple Binary Encoding | Version 1.0 with Errata, Technical Standard, November 2020 ([docs/sbe.md](sbe.md)) | [Simple_Binary_Encoding_V1.0_with_Errata_November_2020.pdf](https://www.fixtrading.org/wp-content/uploads/download-manager-files/Simple_Binary_Encoding_V1.0_with_Errata_November_2020.pdf) | `894f485695b00c83967b56795758607408dd884b4d56a4f9e9c8d5ec8e92dba7` |

SL defines session profiles for FIX.4.2, FIX.4.4 and FIXT.1.1 only
(§4.2.1), and lets an acceptor support "FIX versions prior to FIX.4.4".
fix2 applies SL to FIX 4.0, 4.1 and 4.3 sessions too, for the fields
those versions have, as do the FIXT test cases, "applicable to all
versions of FIX 4.X (4.0, 4.1, 4.2, 4.3, and 4.4) except where
explicitly indicated".

SL and FIXP read their keywords per RFC 2119 (SL §2, FIXP §1.2), as
does TV (§2).

## Classification

| Class | What the text says | fix2 |
|---|---|---|
| MUST | must, shall, required, must not | the default. A peer's departure is refused; accepting it takes a `Tolerate` option |
| SHOULD, MAY, implementation choice | should, recommended, may, optional, by counterparty agreement, or nothing | an option without prefix, or a fixed choice; documented under [Implementation choices](#implementation-choices) |
| Contradiction | two passages, or the text and its examples, disagree | fix2's reading, under [Contradictions](#contradictions); not a toleration |

## Tolerations

### FIX tagvalue

| Option | Accepts or does | Reference | In `QuickFIXTolerations()` | Status |
|---|---|---|---|---|
| `TolerateLogonWithoutEncryptMethod` | an initiator accepts a Logon answer without EncryptMethod (98), as Binance's market data sends | SL §4.3.3: "EncyptionMethod(98) must be present in the Logon(35=A) message and set to “0” when application layer encryption is not in use." | no | done, S1 |
| `TolerateUnsupportedDefaultApplVerID` | accepts a FIXT Logon whose DefaultApplVerID (1137) has no dictionary, reading the peer's messages with our default version | SL §5.3.5: "FIX session processors must maintain state information regarding the default application version used during the FIX session." SL §11.11 has code 18, "Invalid or unsupported Application Version" | yes | done, S1 |
| `TolerateLogoutBeforeGapRecovery` | answers a Logout received ahead of a gap at once, the gap unrecovered | SL §4.8.8: gaps "must be processed differently" for session messages; for a Logout: "Send ResendRequest(35=2) message requesting missing messages, receive and process retransmission of missing messages and gap fills, then send the Logout(35=5) acknowledgement" | yes | done, S1 |
| `TolerateResetLogonSeqNumNotOne` | accepts a first Logon, or Logon answer, with ResetSeqNumFlag (141) = Y whose MsgSeqNum (34) is not 1, as a gap after the reset (a reset Logon on an established session must be 1 whatever the option) | SL §4.1: "A FIX session shall start with a next expected outgoing sequence number (NextNumOut) of 1". SL §4.4.2: "send a Logon(35=A) message with ResetSeqNumFlag(141) set to “Y” and MsgSeqNum(34) set to 1" | no | done, S2 |
| `TolerateMalformedNextExpectedMsgSeqNum` | ignores, with a warning, a NextExpectedMsgSeqNum (789) on Logon that is not a SeqNum, when the session honours 789, and lets the session dictionary's checks pass it, instead of refusing the Logon (SessionRejectReason 6 on a reset Logon of an established session) | TV §6.2.2, SeqNum: "Sequence of character digits without commas or decimals. Value must be positive." SL §4.5: "The initiator and acceptor shall validate incoming FIX messages to ensure proper tagvalue encoding." | no | done, S2 |
| `TolerateTagLeadingZeros` | the parser accepts tag numbers with leading zeros (`035=D`, read as 35), tag 0 refused still; an option of the parser, `fix.ParseOptions`, whose methods `Parse`, `ParseInto` and `ParseFromReader` parse as the functions of those names, and of the session, `Tolerations`, which parses what it receives with it. Refused otherwise as a malformed tag: a logged on session rejects the message with SessionRejectReason 0, or disregards it as garbled when the tag comes before MsgType or MsgSeqNum | TV §6.2.2, TagNum: "Sequence of character digits without commas or decimals. Value must be positive and may not contain leading zeros." | no | done, S2 |
| `TolerateSequenceResetWithoutOrigSendingTime` | accepts a PossDup SequenceReset without OrigSendingTime (122), as some venues send | FIXT, Standard Message Header, OrigSendingTime: "Required for message resent as a result of a ResendRequest. If data is not available set to same value as SendingTime". SL §4.8.4: a retransmitting processor "must modify" "OrigSendingTime(122) set to the SendingTime(52) from the original message" | no | done, S1 (was `Leniency.SequenceResetWithoutOrigSendingTime`) |
| `TolerateValidationFailures` (`dictionary.Checks`) | skips the dictionary checks it names: unknown or user-defined tags, group field order, and the others of `dictionary.Checks` | SL §4.5: "The initiator and acceptor shall validate incoming FIX messages to ensure proper tagvalue encoding." FIXT test case 14 (Mandatory): an undefined tag, a tag not defined for the message type, a group out of order are each rejected | no | done, S1 (was `SkipChecks`) |
| `TolerateFinerTimestamps` | accepts received timestamps finer than the precision profile of the session's BeginString (see [Implementation choices](#implementation-choices)): SendingTime (52), OrigSendingTime (122), and the UTCTimestamp and UTCTimeOnly fields the dictionaries check, each by its own version (`dictionary.CheckTimestampPrecision`, which `TolerateValidationFailures` may name alone). Refused otherwise as an incorrect data format, SessionRejectReason 6, a first Logon without an answer | TPA, Table 1: the finer precisions amend "FIX versions 4.2, 4.4, 5.0, 5.0 SP1 and 5.0SPs" only; TPA p. 2: "Bilateral agreement in the FIX Rules of Engagement are necessary for time precision greater than 3 decimal places" | no | done, S2 (`TolerateTimestampPrecision` while planned) |
| `TolerateEmptyRequiredGroups` | accepts a NumInGroup of 0, without entries, for a group the dictionaries require, such as an empty book's MarketDataSnapshotFullRefresh with NoMDEntries = 0 (no venue verified to send it); the session leaves out `dictionary.CheckEmptyRequiredGroups`, which `TolerateValidationFailures` may name alone. Other count errors, `dictionary.CheckGroupCounts`, are still rejected | TV §4.3.6.4: the NumInGroup field "is required and must be larger than zero if the repeating group is required" | no | done, S2 |
| `TolerateNoHeartbeatAwaitingTestRequest` | sends no Heartbeat while our TestRequest awaits its answer, as QuickFIX does: its script `6_SendTestRequest` expects the disconnection right after the TestRequest | SL §4.5.1: "The FIX session processor must send a HeartBeat(35=0) message to the peer during periods of inactivity" | yes | done, S2 |

`QuickFIXTolerations()` returns a fresh value enabling the rows marked
yes, each of which some of the QuickFIX/Go acceptance scripts the
conformance suite runs (`FIX2_QUICKFIX_TESTS`) fail without. Those
scripts also need two implementation choices, `ServeResendRequestSeqTooLow`
and `UnannouncedResetOnLogon`, a `LogoutTimeout` of 2s, QuickFIX's
default, and, where the texts contradict each other, FIXT's reject reason
for an undefined tag, `RejectUndefinedTagAsInvalidTagNumber`, which the
suite sets beside the preset.
The QuickFIX/Go suite and the contribs opt in to what they need.

### FIXP, SOFH and SBE

No preset is planned for FIXP.

| Option | Accepts or does | Reference | Status |
|---|---|---|---|
| `ToleratePeerWithoutKeepalives` | keeps the connection when the peer stays silent beyond its keepalive interval, as Silverflash does ([docs/fixp.md](fixp.md#silverflash-spike)) | FIXP §4.5: "Each peer must send a heartbeat message during each interval in which no application messages were sent." | planned, S4 |
| `TolerateTrailingBytes` | accepts bytes after the SBE message within its frame, when the message header's version is not above the schema's, as Silverflash pads its session messages; the generated codecs return the length they consumed, block padding counted, since S3 ([docs/sbe.md](sbe.md#decoding)) | SOFH §2.1.1: Message_Length is "the length in octets (i.e. bytes) of a message inclusive of the length of the Simple Open Framing Header". (to confirm): SOFH bounds the frame, not what the SBE message inside it consumes; no SBE provision found yet forbidding the bytes, without which this is a validation policy, not a toleration | consumed length done, S3; the option planned, S4; today accepted, hard-coded ([docs/fixp.md](fixp.md#limits)) |
| `TolerateForeignMetadata` (`fix2gen sbe -tolerate-foreign-metadata`) | loads an SBE schema holding elements and attributes of namespaces other than SBE's, such as a venue's annotations, and ignores them; XML Schema instance's location hints, `xsi:schemaLocation` and `xsi:noNamespaceSchemaLocation`, are accepted without it, XML Schema allowing them on any element; `xsi:nil`, `xsi:type` and other `xsi` names are refused with it or not | SBE §6.1: "See sbe.xsd for the normative XML Schema Definition (XSD) for SBE." sbe.xsd declares no `xs:any` nor `xs:anyAttribute` | done, S3 |
| `TolerateUnfilledCounts` (`fix2gen sbe -tolerate-unfilled-counts`) | takes a header's or dimension's `numGroups` or `numVarDataFields` of 0, where the received version holds groups or data, for a count left unfilled, as Real Logic's encoders leave them, rather than refusing the message as malformed: the decoder reads what it knows the version holds. In a newer version, such a count tells nothing of its groups and data: `ErrNewer` where they could come before what the decoder reads, unless `-assume-same-encodings` too, with which they are taken to be none, a newer version's group before known data then misread | SBE §7.3.2: "This supports a walk by a decoder of all the elements of a message"; a count other than the elements the version holds contradicts it | done, S3 |

## Strictness without option

What fix2 does, or will do, to conform, with no option: invalid input
refused, or valid input accepted where fix2 refused it. A known peer
needing the old behaviour would get a toleration, and a row above.

### FIX tagvalue

| Behaviour | Reference | Status |
|---|---|---|
| Session messages are validated against the session dictionary, before any sequence reset or durable state change, with the narrow exceptions the tolerations above need (EncryptMethod, NextExpectedMsgSeqNum, OrigSendingTime); a Logon refused before logon disconnects without Logout, as before, an established session rejects, counting the message if it is the one expected and not acting on it (a Logout is not answered, a ResendRequest not served, a SequenceReset moves nothing, a reset Logon logs out), a sequence error logs out | SL §4.5: "The initiator and acceptor shall validate incoming FIX messages to ensure proper tagvalue encoding." SL §4.3.1: an invalid Logon request means the "connection terminated without Logout(35=5) processing". SL §4.5.4: "Rejected messages must be logged and NextNumIn incremented by 1." | done, S2 |
| `Dict.Validate`, and so `ParseAndValidate` with a dictionary, run every check, `dictionary.CheckAll`: the unknown tags' only before; `ValidateWith` runs those it is given | SL §4.5, as above | done, S2 |
| `Parse` and `ParseInto` refuse bytes after CheckSum (10), with `ErrBodyLenMismatch`; `RewriteForResend` relies on them for it | TV §4.3.4: "Messages are effectively delimited by the <SOH> character at the end of the CheckSum(10) field." SL §4.5.2: garbled if "Checksum(10) is not the last tag" | done, S2 |
| A message below the next expected MsgSeqNum whose PossDupFlag (43) is hidden by a malformed header tag logs out, as one without PossDupFlag = Y; a garbled frame is still disregarded | SL §4.8.1, as for `ServeResendRequestSeqTooLow`; SL §4.5.2: "The receiving FIX session processor must disregard the garbled message and not increment NextNumIn." | done, S2 |
| A DayOfMonth value outside 1 to 31 is refused, as an incorrect data format (SessionRejectReason 6) with the other formats, `dictionary.CheckFormats` ([`checks.go`](../fix/dictionary/checks.go)); leading zeros are accepted, as in any int (unverified) | TV §6.2.2, DayOfMonth: "Day number within a month (values 1 to 31)" | done, S2 |
| A tag the dictionary does not define is rejected with SessionRejectReason 3, one it defines but not for the message type with 2, as before, a malformed tag number with 0 ([`validate.go`](../fix/dictionary/validate.go)); FIXT's test case 14a gives 0 for an undefined tag, and `RejectUndefinedTagAsInvalidTagNumber` keeps that answer for peers expecting it (see [Contradictions](#contradictions)) | SL §11.11: "InvalidTagNumber 0 Invalid Tag Number", "UndefinedTag 3 Undefined tag" | done, S2 |
| A Heartbeat is sent in every idle interval, a TestRequest of ours outstanding or not, unless `TolerateNoHeartbeatAwaitingTestRequest` | SL §4.5.1: "The FIX session processor must send a HeartBeat(35=0) message to the peer during periods of inactivity" | done, S2 |
| NumInGroup = 0 is judged by context, not refused or accepted wholesale: a required group needs entries, its 0 refused with SessionRejectReason 16 as any count error, by its own check, `dictionary.CheckEmptyRequiredGroups` ([`checks.go`](../fix/dictionary/checks.go)), unless `TolerateEmptyRequiredGroups`; an optional group's 0, without entries, is accepted (see [Contradictions](#contradictions)). A group is required as the dictionary's `GroupDef.Required` says, in a message or a group entry, or, in an optional component, once the component is present, any of its fields showing it, the group's counter included. QuickFIX's FIX 4.4 dictionary ([`FIX44.xml`](../fix/tests/benchmarks/spec/FIX44.xml)) declares 25 groups required, among them NewOrderMultileg's NoLegs, MarketDataSnapshotFullRefresh's NoMDEntries and Confirmation's NoUnderlyings, NoLegs and NoCapacities, which counterparties may send empty (unverified) | TV §4.3.6.4: the NumInGroup field "is required and must be larger than zero if the repeating group is required", or if it is optional and "the message contains one or more instances" | done, S2 |
| Timestamps follow the precision profile of the session's BeginString, inbound and outbound alike: a received SendingTime or OrigSendingTime finer than the version allows, or a UTCTimestamp or UTCTimeOnly field finer than its dictionary's version allows, is an incorrect data format (SessionRejectReason 6), unless `TolerateFinerTimestamps`; a `Timestamps` finer than the version allows is a configuration error, where [`config.go`](../fix/session/config.go) clamped it. A FIXT.1.1 session takes the profile of FIX 5.0 and later, whatever the ApplVerID of its messages; the body fields of its application messages follow their application dictionary's version, the standard header and trailer and the session messages the transport's (`dictionary.Compose`). A FIX 4.0 or 4.1 dictionary's TIME has whole seconds under `dictionary.CheckTimestampPrecision`, its syntax only under `dictionary.CheckFormats` | TPA, Table 1 | done, S2 |

### FIXP, SOFH and SBE

| Behaviour | Reference | Status |
|---|---|---|
| A RetransmitRequest for sequence numbers never sent terminates the session, unless `RejectRetransmitRequestBeyondSent`; one for messages no longer kept still gets RetransmitReject | FIXP §6.3, as for `RejectRetransmitRequestBeyondSent`; FIXP §4.6.2.3: "If the provider of a recoverable flow is unable to retransmit requested messages, it responds with a RetransmitReject message." | planned, S4 |
| `NextSeqNo` is read four ways, Establish or EstablishmentAck, sent or received: EstablishmentAck carries it for a Recoverable server flow, first establishment included; an Idempotent flow starts with a Sequence; Establish follows the reading of §4.3.1 under [Contradictions](#fixp) | FIXP §4.3.2: "For a recoverable server flow only, the next application sequence number to be produced by the server." FIXP §4.8: "The start of an idempotent flow must be initiated with a Sequence message" | planned, S4 |
| A Terminate answering one whose code is unknown carries `UnspecifiedError`, never the unknown code | FIXP §4.4: "TerminationCode = Finished \| UnspecifiedError \| ReRequestOutOfBounds \| ReRequestInProgress". SBE §4.15: "A value is invalid if it does not match one of the explicitly listed valid values." | planned, S4 |
| A session message with a null in a required field, or an unknown enumeration value, is malformed: the connection terminates | SBE §4.15: "The null value of a datatype is invalid for a required field." FIXP §4.4: "a transport should be terminated if an unrecoverable error occurs in message parsing or framing." | planned, S4 |
| An Applied received for our flow when it is not Idempotent, or beyond what we sent, is a protocol violation. (to confirm): FIXP states the violation for NotApplied only (§4.8.2: "Sending NotApplied for a Recoverable, Unsequenced or None flow is a protocol violation."); for Applied it says only what it is for | FIXP §4.8.1: "This is an optional application response message to support an idempotent flow." | planned, S4 |
| A MessageTemplate during the handshake is ignored, without extending the handshake's deadline (accepted where it was refused) | FIXP §3.6: "MessageTemplate may be sent at any time." | planned, S4 |
| Several Retransmission batches answer one RetransmitRequest, sent and accepted, while a peer answering with one partial batch and expecting a new request for the rest is still served (accepted where it was refused) | FIXP §4.6.1: "which should be answered by one or more Retransmission messages". FIXP §4.6.2.2: "If not, it should send another RetransmitRequest for the remainder." | planned, S5 |
| After our Terminate, a receive-only phase: inbound messages are processed, nothing is written; exactly one Terminate goes out | FIXP §4.4.1: "the party that initiated termination should then wait for a response from its peer to permit in-flight messages to be processed". FIXP §6.3: "Sending any application or session message after sending Terminate." | planned, S5 |
| Finalization: documented as a contradiction, defaults unchanged until resolved; tested with a FinishedReceiving arriving while recovery of the reverse flow is pending | see [Contradictions](#fixp) | planned, S5 |
| `fix2gen sbe` checks SBE elements and attributes by their expanded names ([`load.go`](../internal/gen/sbe/load.go)): the root is `messageSchema` of SBE 1.0's namespace, other namespaces, SBE 2.0's and the release candidates' among them, refused; `message` of that namespace, the other elements and the attributes of none, each where sbe.xsd declares it; a misspelt SBE attribute, an element out of place, an undeclared prefix refused; metadata of another namespace, elements and attributes, refused unless `TolerateForeignMetadata`; XML Schema instance's location hints accepted, `xsi:nil`, `xsi:type` (which would change validation) and other `xsi` names refused | SBE §6.1: "See sbe.xsd for the normative XML Schema Definition (XSD) for SBE." SBE §6.2: `xmlns:sbe=http://fixprotocol.io/2016/sbe` | done, S3 |
| The header's and dimensions' `numGroups` and `numVarDataFields`, where the schema has them, are the groups and data the received version holds, those whose `sinceVersion` is not above it, in a message of any version: another count is `ErrMalformed`, unless `TolerateUnfilledCounts` for a count of 0, or a newer version's count above them (next row) | SBE §7.3.2: "This supports a walk by a decoder of all the elements of a message" | done, S3 |
| Decoding a newer schema version: fixed-block extensions are skipped by `blockLength`, as before; groups and data the schema does not know are `ErrNewer`, their encodings unknown: where the header or a dimension counts them, a count above the schema's; where nothing counts them, wherever they could come before what the decoder reads (groups before a message's data, anything in a group's entries). After all the decoder reads, uncounted, they cannot be told from bytes after the message: left unread, out of the length returned, the caller deciding. `-assume-same-encodings` skips counted ones (see [Implementation choices](#implementation-choices)) ([docs/sbe.md](sbe.md#decoding)) | SBE §7.3.2: "This supports a walk by a decoder of all the elements of a message". SBE §7.1.1, §5.4.10.2: a newer version's fields and groups may have types and dimensions of their own | done, S3 |

## Implementation choices

Options and fixed behaviours without prefix, each where the text leaves
the choice open.

### FIX tagvalue

| Choice | fix2 | Reference |
|---|---|---|
| HeartBtInt (108) = 0 | no regular heartbeats on that connection, a TestRequest still answered with its Heartbeat; an acceptor takes 0 whatever its range | FIXT, Heartbeat: "If HeartBtInt is set to zero then no regular heartbeat messages will be generated." |
| A ResendRequest whose MsgSeqNum (34) is too low, without PossDupFlag (43) = Y | logs out unserved; `ServeResendRequestSeqTooLow` serves it first, then logs out, as QuickFIX does and its acceptance tests expect (since S1) | SL §4.8.1: "The FIX session should be terminated if the incoming message MsgSeqNum(34) is less than NextNumIn and PossDupFlag(43) is not set to “Y”" |
| Announcing a reset made for `ResetOnLogon` | an acceptor answers a Logon without ResetSeqNumFlag (141) with one, at MsgSeqNum 1, when the reset moved its sequence numbers; `UnannouncedResetOnLogon` answers without it, as QuickFIX's acceptance tests expect, a peer that did not reset then logging out "MsgSeqNum too low" (since S1) | SL §4.4.3: "Counterparties must agree to support the reset of a FIX session during FIX connection establishment." No text requires the answer to an unasked reset to carry 141 |
| A Logon answer without ResetSeqNumFlag (141) to our reset Logon | accepted, its MsgSeqNum telling whether the peer reset (Binance does not echo it) | SL §4.4.2: the peer "should set its NextNumIn to 2 and NextNumOut to 1, then send a Logon(35=A) acknowledgement with ResetSeqNumFlag(141) set to “Y”"; FIXT: "The acceptor should respond with a Logon with ResetSeqNumFlag set to Y" |
| Transport security | fix2 runs over whatever `transport` it is given: TCP, TLS or in memory. It does not implement FIXS, nor refuse plain TCP; whether a FIX session over TCP uses FIXS is the caller's responsibility, outside what fix2 guarantees: fix2's conformance claims stop at the session layer | SL §4.3.1: "FIX implementations that use the TCP/IP protocol as the transport layer must use the FIX-over-TLS (FIXS) specification". FIXS (Draft Standard) is not pinned |
| HeartBtInt range of an acceptor | `MinHeartBtInt`, `MaxHeartBtInt`, 1s and 300s by default | SL §4.3.5.2: "The acceptor may refuse to establish a FIX connection if the HeartBtInt(108) value ... does not fall within a heartbeat interval range" |
| Timestamp precision | `Timestamps`, milliseconds by default, within a profile per BeginString ([`version.go`](../fix/session/version.go)): whole seconds for FIX.4.0 and FIX.4.1, milliseconds for FIX.4.3, which TPA does not amend, picoseconds for FIX.4.2, FIX.4.4 and FIXT.1.1, of which a session writes nanoseconds at most. Since S2, a finer `Timestamps` is a configuration error rather than clamped, FIX.4.2 and FIX.4.4 may go finer than milliseconds, and received timestamps are checked against the profile (see [Strictness without option](#strictness-without-option)). FIX 4.0 and 4.1 precisions are (unverified) against their base texts | TPA p. 2: "Bilateral agreement in the FIX Rules of Engagement are necessary for time precision greater than 3 decimal places"; p. 5: "FIX Service providers are encouraged to make the high-resolution precision levels configurable." |
| SessionRejectReason for an undefined tag | 3, Undefined tag, by default; `RejectUndefinedTagAsInvalidTagNumber` answers 0, Invalid tag number, as FIXT's test case 14a and QuickFIX's acceptance tests expect (since S2) | the texts contradict each other: see [Contradictions](#contradictions) |
| SendingTime (52) accuracy | `MaxLatency`, 120s by default, the FIXT test cases' example | SL §4.2.3: "The SendingTimeThreshold may be defined and communicated to counterparties out-of-band within the rules of engagement." FIXT test case 2n: "within a reasonable time (e.g. 2 minutes)" |
| TestRequest after silence, and the wait for its answer | HeartBtInt plus a fifth each, hard-coded | SL §4.5.5: "A reasonable TestRequestThreshold is anywhere from 1.2 to 2.0"; FIXT test case 6: "use 20% of HeartBeatInt field" |
| Wait for the Logout answer | `LogoutTimeout`: by default twice the heartbeat interval in effect (that agreed at logon, `HeartBtInt` before), as SL recommends, 2s when that interval is 0 (since S2; 2s before, the wait of the FIXT test cases, which set no requirement). The conformance suite sets 2s, QuickFIX's default, which its scripts' waits need | SL §3.1.27, LogoutAckThreshold: "This value may be specified out-of-band in a rules of engagement." SL §4.6: "Twice the value specified in the HeartBtInt(108) field of the Logon(35=A) message is recommended." |
| Wait for the Logon | `LogonTimeout`, 10s by default | SL sets no threshold for it |
| Resynchronising after a garbled frame | `Framer` resynchronises on the stream after garbage | SL §4.5.2: "The receiving FIX session processor must disregard the garbled message and not increment NextNumIn." How to find the next message is not specified |
| Store retention | `Retention`: evicted messages are gap-filled when asked for again, and the handler told | SL §4.8.3: "The peer receiving the ResendRequest(35=2) may choose not to retransmit certain application messages." |
| ResendRequest ranges | `ResendChunkSize`, everything at once by default | SL §4.8.2: "The ResendRequest(35=2) may be used to request retransmission of a single message, a range of messages or all messages subsequent to a particular message." |
| NextExpectedMsgSeqNum (789) on Logon | `NextExpectedMsgSeqNum`, off by default | SL §4.4.1: "by counterparty agreement defined in the FIX session rules of engagement" |
| LastMsgSeqNumProcessed (369) | `LastMsgSeqNumProcessed`, off by default | FIXT, Standard Message Header: "Can be specified on every message sent." |
| Sequence resets at logon, logout, disconnection | `ResetOnLogon`, `ResetOnLogout`, `ResetOnDisconnect`, off by default | SL §4.4.3: "Counterparties must agree to support the reset of a FIX session during FIX connection establishment." |
| Queues, stores, writes, delivery | `MaxPending*`, `MaxStash*`, `DeliveryQueue*`, `StoreTimeout`, `WriteTimeout` and the like | outside the specifications: resource bounds |

### FIXP, SOFH and SBE

| Choice | fix2 | Reference |
|---|---|---|
| Leniency on the peer's keepalive interval | `KeepaliveLeniency` (today `Leniency`), 1.5 by default, no cap; S4 rejects NaN, infinities and overflow | FIXP §4.4: "It is recommended to allow some leniency in timeout to allow for slight mismatches of timers between parties." |
| Keepalive intervals accepted | a server bounds the client's (`MinKeepalive`, `MaxKeepalive`); S4 lets a client bound the server's too | FIXP §4.3.1: "Counterparties may agree on a valid range for KeepaliveInterval." |
| Wait for NegotiationResponse and EstablishmentAck | `HandshakeTimeout` | FIXP §4.3.1: "There is no specific timeout value for the wait defined in this protocol." |
| Retransmission batch size | `RetransmitBatch` | FIXP §4.6.2.2: "The provider of a recoverable flow need not retransmit all requested messages in a single batch." |
| How long finalized session IDs are refused | `Retention` | FIXP §6.3: "(The server may have a practical limit of session history to enforce this rule.)" |
| Negotiate and Establish timestamps | `MaxClockSkew` | the text sets no bound; examples only (FIXP §7.1.7.4, §7.2.5.6) |
| A RetransmitRequest for sequence numbers never sent | Terminate, from S4; `RejectRetransmitRequestBeyondSent` (name provisional) answers RetransmitReject `OutOfRange` instead, as the examples do (FIXP §7.4.5.1, §7.4.5.2) | FIXP §6.3: "it is a violation to request a retransmission of a message with a sequence number that has not been sent yet", and of every violation: "the session should be immediately terminated" |
| Applied | sent only when the application sends it | FIXP §4.8.1: "This is an optional application response message to support an idempotent flow." |
| MessageTemplate content | ignored, in-band templates being out of scope ([docs/fixp.md](fixp.md#profile)) | FIXP §3.6: "MessageTemplate may be sent at any time." |
| Frame length, stored messages, sessions, credentials, unestablished sessions | `MaxFrameLength`, `MaxStoredMessages`, `MaxSessions`, `MaxCredentialsLength`, `UnestablishedTimeout` and the like | outside the specifications: resource bounds |
| Skipping a newer version's counted groups and data | refused, `ErrNewer`, by default; `fix2gen sbe -assume-same-encodings` skips them by the schema's `groupSizeEncoding` and data length, an assumption SBE does not guarantee, under which a newer version encoding them otherwise is misread (since S3) | SBE §7.3.2: the counts "support a walk by a decoder"; how to walk elements of unknown types the text does not say. SBE §7.1.1, §5.4.10.2: new fields may have new types, a group another `dimensionType` |

## Contradictions

### FIX tagvalue

| Point | One text | The other | fix2 |
|---|---|---|---|
| EndSeqNo (16) = 0 | TV §6.2.2, SeqNum: "Value must be positive." | SL §10, EndSeqNo, a SeqNum: "EndSeqNo(16) = “0” (representing infinity)." | 0 in EndSeqNo means infinity, as SL says |
| NumInGroup = 0 | TV §6.2.2, NumInGroup: "Value must be positive." | TV §4.3.6.4 requires a count "larger than zero" only for a required group or one with entries | read by context, §4.3.6.4 taken as qualifying the data type's rule for counters: a required group's 0 is refused (SessionRejectReason 16, the count being what the text constrains, as for any count error), an optional group's 0 without entries accepted (since S2; see [Strictness without option](#strictness-without-option)) |
| Reject reason for an undefined tag | SL §11.11: "UndefinedTag 3 Undefined tag" | FIXT test case 14a, "Receive field identifier (tag number) not defined in specification": SessionRejectReason "Invalid tag number" (0) | since S2, SL's code list: 3 for a tag the dictionary does not define, 2 for one it defines but not for the message type, 0 for a malformed tag number; the test case predates SL's list, and `RejectUndefinedTagAsInvalidTagNumber` answers 0 for an undefined tag, for peers expecting it |
| Strength of the "MsgSeqNum too low" rule | SL §4.8.1: "should be terminated"; FIXT, Message Recovery: "It is strongly recommended that the session be terminated" | FIXT test case 2c, in a test marked Mandatory: Logout, disconnect, an "error" condition | SL's "should", which the test case illustrates: fix2 logs out by default, and serving a too-low ResendRequest first, `ServeResendRequestSeqTooLow`, is an implementation choice |
| BeginString of a garbled message | SL §4.5.2: garbled if BeginString "is not one of the defined FIX session profile identifiers (“8=FIX.4.4”, “8=FIXT.1.1”)" | SL §4.2.1, Table 1, defines a FIX.4.2 profile too | the list reads as incomplete: FIX.4.2, and the earlier FIX 4.x versions fix2 supports, are valid |

### FIXP

[docs/fixp.md](fixp.md#where-the-standard-contradicts-itself) lists
the contradictions of FIXP 1.0 and fix2's reading of each. The
strict-by-default effort adds or changes these:

| Point | The text | The other passage or the examples | fix2 |
|---|---|---|---|
| Establish `NextSeqNo` | FIXP §4.3.1: "For re-establishment of a recoverable server flow only, the next application sequence number to be produced by the client." A server flow's numbers are the server's | the schema: "for re-establishment of a recoverable flow only" ([docs/fixp.md](fixp.md#the-schema)) | the value is the client's next sequence number on its own flow; when it is sent and accepted is settled in S4 |
| Finalization | FIXP §4.7.2: "When a FinishedReceiving has been received by the party that initiated the finalization handshake, a Terminate message should be sent to unbind the transport." FIXP §6.3: "The responder on a point-to-point session may not send an application message after sending FinishedReceiving." | FIXP §7.5.1: "the FIXP session could be terminated once both counterparties have sent and received a FinishedReceiving message." FIXP §7.5.6: the half-closed side "should still be ready and able to accept messages from the other counterparty" | as docs/fixp.md says: both flows finished, then either side terminates. Unchanged until resolved; S5 adds the case of a FinishedReceiving arriving while recovery of the reverse flow is pending |
| Batches answering one RetransmitRequest | FIXP §4.6.1: "one or more Retransmission messages" | FIXP §4.6.2.2: the requester "should send another RetransmitRequest for the remainder"; FIXP §7.4.4.2: a second batch under the first request's `RequestTimestamp` | from S5, several batches per request, sent and accepted, a peer re-requesting the rest of a partial batch still served; docs/fixp.md's row changes then |
| RetransmitRequest beyond what was sent | FIXP §4.6.4, §6.3: a protocol violation | FIXP §7.4.5.1, §7.4.5.2: RetransmitReject `OutOfRange` | from S4 the text: Terminate; the examples' answer behind `RejectRetransmitRequestBeyondSent`, an implementation choice since §6.3 says "should"; docs/fixp.md's row changes then |

## Changing this document

Every new leniency, a `Tolerate` option or a hard-coded acceptance of
what a pinned text forbids, gets a row here, with its section and a
quote, before it is merged. So does every new option without prefix,
with the text that leaves the choice open, and every new reading of a
contradiction. A new edition or addendum is pinned in the table above,
with its hash, before code relies on it.

## Milestones

| # | Content | Done |
|---|---|---|
| S0 | This document | yes |
| S1 | `fix/session`: `Tolerations`, `QuickFIXTolerations()`, `Compatibility` removed, the renames; conformance and QuickFIX/Go suites and contribs opt in | yes |
| S2 | `fix/` strictness: the FIX rows planned for S2, the parser options API | yes |
| S3 | `fix2gen sbe`: consumed length, namespaces, newer versions; [docs/sbe.md](sbe.md) | yes |
| S4 | `fixp/` options and the simple FIXP rows | no |
| S5 | FIXP lifecycle: several batches per request, the receive-only phase after Terminate, finalization | no |
