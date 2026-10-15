# Fourth audit: parser, dictionary, builders, generator

Audited HEAD: `87fad5a6c5ce0388f204b5210b4fb6a5abe90672`.
Environment: Go 1.27.2, Darwin arm64. Source and existing tests unchanged.

## Confirmed conditional finding

`dictionary/profile.go:110-117` falls back after `fix2.NewProfile(pairs)` rejects a pair outside its supported tag range. It filters pairs by constructing a complete `NewProfile` for each pair (`profile.go:45` allocates one int32 per tag up to that pair's maximum). Every valid pair therefore causes another dense allocation, although the filter needs only range validation. Adding one unused out-of-range pair triggers all this work.

The public-API reproduction is `profile-fallback-cost_test.go.txt`. Copy it to `dictionary/xmldict/audit4_profile_cost_test.go` in an isolated HEAD archive and run:

```
GOCACHE=/tmp/fix2-audit4-parser-go-cache go test -count=1 -run '^TestAudit4ProfileFallbackCost$' -v ./dictionary/xmldict
```

`profile-fallback-cost.log` records the observed amplification. With 4,096 pairs around tag 900,000, the 444,412-byte XML including an out-of-range pair allocates 14,844,849,920 bytes cumulatively and takes 1.55 seconds; without that 109-byte pair it allocates 14,816,760 bytes and takes 17 milliseconds. The loader accepts both. These are diagnostic timings, not stable benchmark thresholds. Cumulative allocation is not peak resident memory.

Exposure requires loading custom dictionaries controlled by an attacker or supplied through a tenant/runtime dictionary feature. Ordinary FIX wire messages cannot trigger dictionary compilation. Trusted fixed bundled dictionaries do not demonstrate this condition. Conditional P2 for such runtime import services; otherwise P3 robustness/startup concern, not a general production blocker.

Correction: validate range without allocating a dense profile per pair, then construct the final profile once; or reject unsupported pairs explicitly during loading. Preserve the intended supported-pair semantics. A budget regression for a high-tag dictionary plus one unsupported pair should bound allocation to input/metadata plus one supported tag table, rather than pair count times highest tag.

## Prior T02 remediation

`ae7afca` covers all three width-dependent costs: wide positions are indexed, per-entry bitsets clear touched words, and optional component activation starts from bits present rather than scanning absent component tags. Targeted wide/nested/component/shared-counter regressions passed. `t02-regressions.log` records flat local timings for 20,000 two-field entries: schema widths 64, 4,096 and 65,536 took 1.78, 1.53 and 1.68 ms respectively.

## Other review and checks

Reviewed parser bounds, DATA/LENGTH adjacency and body limits, framing/recovery, builder section/group serialization, dictionary required-field and optional-component compilation, group order/count checks, generator naming and contextual layouts. No other production defect confirmed in this slice.

Normal and race suites passed: root, dictionary, XML dictionary, generator (see `focused-race.log`). Short focused fuzz campaigns passed in the isolated archive: Groups 359,587 executions; XML Load 66,693 executions; Framer 452,531 executions. Logs are preserved alongside this note. Passing fuzzing is bounded evidence, not completeness.
