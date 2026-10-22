# Seventh audit: contrib clients and SBE

Revision: `8e9a7f4` (isolated `git archive HEAD` at `/private/tmp/fix2-audit7-clients`). Go 1.27.2, macOS arm64.

## Confirmed finding: SBE trade JSON omission (proposed P3)

`contribs/binancemd/output.go:221-223` assumes every event JSON-encodes and discards `Encoder.Encode`'s error. The new SBE trade path at `contribs/binancemd/sbe.go:339` constructs a `time.Time` directly from a signed 64-bit microsecond `TransactTime`. The codec preserves its full range and `contribs/binancefix/marketdata.go:672-675` rejects only the null timestamp (`math.MinInt64`). Other values can produce years outside JSON time's supported 0..9999 range. The event is silently omitted; later ordinary events continue to print and cancellation returns success.

Reproduction: `sbe-trade-timestamp_test.go.txt`, executed only as an added scratch-archive test. A fake Binance market-data endpoint logs the client on and receives its subscription. Three trades are broadcast: ordinary ID 1, candidate timestamp ID 2, ordinary ID 3. Seeing ID 3 is a serialized-handler barrier; then the command is canceled and its error inspected. Ordinary and last-second-of-year-9999 controls print all three lines and return nil. Both `math.MaxInt64` and `math.MinInt64+1` print only IDs 1 and 3, while returning nil. Three race-enabled repetitions show the same result with no race warning. `sbe-trade-timestamp.log` preserves the deliberately failing output.

Impact/limits: this demonstrates silent data loss for erroneous upstream timestamps in `binancemd --sbe --json trades`. No ordinary current timestamp loss, live Binance incident, order-entry receipt failure, or trading loss was demonstrated. This is a narrow robustness defect, not a general production blocker. Retain schema codec behavior: the command should either reject an unsupported timestamp explicitly or surface JSON encoding failure through its existing output/command error path. The library need not impose JSON's range on a schema integer.

Re-run after copying the saved diagnostic into the archived module:

```sh
go test -race -count=3 -run '^TestAudit7SBETradeTimestampOutput$' -v .
```

## Checks

All checks were run against the unmodified archive before adding the diagnostic (except the diagnostic itself). Initial ordinary test invocations were denied access to Go's default build cache; permitted reruns are the successful evidence here.

| Command / working directory | Result / evidence |
|---|---|
| `go test -race -count=1 ./...`, `contribs/binancefix` | Passed; `audit7-binancefix-race.log` |
| Same, `contribs/binancemd` | Passed; `audit7-binancemd-race.log` |
| Same, `contribs/coinbasemd` | Passed; `audit7-coinbasemd-race.log` |
| Same, `contribs/fixpecho` | Passed; `audit7-fixpecho-race.log` |
| `go test -race -count=1 ./internal/gen/sbe/...`, repository root | Passed; `audit7-sbe-race.log` |
| `go test -run '^$' -fuzz '^FuzzSBEClient$' -fuzztime=15s`, `contribs/binancemd` | Passed: 201,105 executions, `audit7-sbeclient-fuzz.log` |
| `go test -run '^$' -fuzz '^FuzzDecode$' -fuzztime=15s`, `internal/gen/sbe/tests/wide` | Passed: 2,692,172 executions, `audit7-sbedecode-fuzz.log` |

Read-only source review covered Binance client connection/logon/logout/reconnect behavior, admin/request queues, receipts/leg correlation, subscription ownership and rate accounting, framing/trace redaction, SBE generated decoder bounds, Binance market-data SBE dispatch/book/output, Coinbase book/output deltas, and FIXP echo client/server/TLS behavior. No additional concrete defect was confirmed in those areas. This was focused review, not exhaustive line-by-line inspection of all generated code or a live exchange/soak/security campaign. The default offline suites do not run opt-in Binance testnet tests. Existing SBE client fuzzing asserts absence of panic; it does not assert preservation of every encoded output event, explaining why this omission escapes that fuzz check.

Only audit evidence was added to the working project. Production sources and permanent tests are unchanged.
