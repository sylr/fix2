# Fifth audit: parser, dictionary, builders, generator

Audited revision: `0da258079503502ece32633a0030bf21e4b2a529`.
Environment: Go 1.27.2, Darwin arm64. No production source or existing tests changed.

## A4-04 remediation verified

`dictionary/profile.go:110-121` filters `!DataPair.InRange()` before constructing a single parsing profile. The old fallback no longer constructs a dense profile per supported pair. The final panic is unreachable for dictionaries accepted by the XML loader: DATA and LENGTH classifications cannot coincide for a tag, and `pair` refuses previously used tags. Unsupported pairs are still omitted intentionally.

The original fourth-audit public-API diagnostic was executed unchanged in an isolated archive at the fifth-audit revision. At 4,096 supported pairs, the plain dictionary allocated 14,822,048 bytes and the outlier dictionary 15,085,344 bytes; the previous outlier consumed approximately 14.84 GB cumulatively. Local times were 14.1/15.0 ms. The current log is `a4-04-remediation.log`; the diagnostic source remains preserved under audit four. These are allocation diagnostics, not peak-RSS measurements or stable timing thresholds.

Current InRange/range-error, dictionary profile inference, profile allocation and wide-group cost regressions passed (`regressions.log`).

## P3: LENGTH-only group definitions have no generated way to write their implied DATA pair

Sources: `cmd/fixgen/generate.go:705-715` iterates only merged declared fields and skips each LENGTH setter in expectation that its DATA setter will write the pair. `cmd/fixgen/generate.go:690-692` keeps the entry's raw fields private. When the group's merged definitions never explicitly list the DATA tag, that DATA setter is never generated.

The library supports implicitly adding either partner to group definitions: dictionary compilation adds both, the parser requires adjacency, and `specLiteral`/`pairDataLengths` include both in the generated group's spec/layout. This is not merely an XML dictionary that happens to load. The reproduction proves that the generated `Dictionary()` strictly accepts a real message with one complete entry, and that the generated reader extracts that entry.

Minimal case: group NoG has RawDataLength(95) as its only declared field; RawData(96) is defined globally but omitted from all NoG definitions. `fixgen` succeeds. Generic wire `500=1|95=3|96=a<SOH>b|` passes `ParseInto`, `ValidateWith(CheckAll)`, and the generated group reader. Yet `NewNoGEntry()` has neither `SetRawData` nor any public raw-field setter, so applications cannot populate its delimiter and DATA value through the generated entry API. A control adding RawData to the group's XML declaration generates `SetRawData` and round-trips successfully.

This is a low-priority generator completeness defect affecting custom schemas that rely on the library's implicit-pair support. No bundled fixture exposure or remote exploit is established. Workaround: explicitly list DATA alongside LENGTH before generation. A correction should expand the effective direct fields used to generate setters/readers to include paired DATA, or fail generation clearly for the unsupported shape. Cover an implicit DATA partner absent from every merged group definition; existing `TestGenerateLengthWithoutData` explicitly lists DATA in another message's definition and therefore does not cover this case.

Evidence: `length-only-group_test.go.txt`, executed as `cmd/fixgen/audit5_length_only_test.go` in the isolated archive; `length-only-group.log`. Run:

```
GOCACHE=/private/tmp/fix2-production-readiness-4-gocache go test -count=1 -run '^TestAudit5LengthOnlyGroup$' -v ./cmd/fixgen
```

The implicit-DATA subtest intentionally fails at the missing generated setter, after logging that its strict-valid message control was accepted and read. The declared-DATA control passes. The preserved run uses `-race` for the generator wrapper; the helper's generated-module child runs ordinary `go test`.

## Additional review and verification

Reviewed parsing limits and malformed fields, DATA framing, stream resynchronization, group boundary/count/order/required/component checks, generation naming/layout merging and builder section handling. No other concrete production defect confirmed in this slice.

Independent 15-second fuzz campaigns passed: Framer 176,198 executions; XML Load 115,165 executions (`fuzz-framer.log`, `fuzz-load.log`). Broad normal/SIMD/race/vet/contrib verification is owned by the audit leader. Short fuzzing is bounded evidence, not completeness.
