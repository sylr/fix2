# Jepsen infrastructure vulnerability scan triage

**The infrastructure scan is not clean: five advisories have symbol-level call traces.** The recorded process exit status of zero must not be interpreted as zero findings. These are indirect dependencies of the separate `jepsen/infra` provisioning module; this evidence does not establish a vulnerability in the production FIX/FIXP library or demonstrate an exploit of the provisioning programs.

Input: `/private/tmp/fix2-production-readiness-7-8e9a7f4/jepsen-infra-vuln.json`, parsed as concatenated JSON objects. Scanner config: govulncheck v1.7.0, Go 1.27.2, source mode, symbol scan level; official Go vulnerability database last modified 2026-10-20 00:20:56 UTC. This triage interprets that existing scan and its embedded OSV records; it does not rescan dependencies or independently reproduce an advisory exploit.

There are 108 finding records, **not 108 distinct vulnerabilities**: six distinct advisory IDs, six module-level records, five package-level records and 97 symbol-level records. Symbol traces use the JSON keys `function` and `receiver`; searching for a `symbol` key misses them. A module-only record reports a vulnerable version present; package records additionally identify an imported affected package; symbol records provide a static call path to an affected function. These levels are cumulative observations of the same advisory, not additive unique defects.

| Advisory | Embedded advisory summary | Module / pinned version | Fixed version in finding | Records: module / package / symbol |
|---|---|---|---|---|
| GO-2026-5932 | Unmaintained, unsafe OpenPGP package | `golang.org/x/crypto v0.57.0` | None supplied | 1 / 0 / 0 |
| GO-2026-6603 | HTTP/2 server memory exhaustion via Trailer headers | `golang.org/x/net v0.59.0` | `v0.60.0` | 1 / 1 / 24 |
| GO-2026-6610 | Malformed framing-related HTTP/2 headers can enable response smuggling when proxying to HTTP/1 | Same | `v0.60.0` | 1 / 1 / 1 |
| GO-2026-6611 | CPU exhaustion via repeated HTTP/2 initial-window changes | Same | `v0.60.0` | 1 / 1 / 24 |
| GO-2026-6612 | HTTP/2 server flow-control refund bypass | Same | `v0.60.0` | 1 / 1 / 24 |
| GO-2026-6617 | HTTP/2 server HPACK encoder race/crash | Same | `v0.60.0` | 1 / 1 / 24 |

The OpenPGP advisory has **no affected-package import or affected-function trace** in this scan; it is a module presence observation. The five `x/net` advisories **do** have affected-function traces. For example, `GO-2026-6610` traces `http2.(*Transport).RoundTrip` through Pulumi HTTP asset fetching/marshalling to `jepsen/infra/multipass/main.go:81` (`pulumi.Run`). Other records trace HTTP/2 frame operations through gRPC/Pulumi. Some traces reach generic `Error`/`String` methods through formatting or JSON marshalling to inventory helpers: those traces should not be described as proof that a malicious HTTP/2 exchange occurs in those helpers.

`jepsen/infra/go.mod` directly requires Pulumi SDK, command and TLS packages; `x/net v0.59.0` is indirect. The repository's infrastructure entry points at `run/main.go:48` and `multipass/main.go:81` run Pulumi programs, not a public-facing project HTTP API. A focused search of project-owned infrastructure Go files found no HTTP reverse proxy or explicit HTTP server/listener implementation. This limits conclusions for the server-only advisories and the reverse-proxy precondition in GO-2026-6610. It does not eliminate dependency-created network services or the client-side HTTP/2 peer condition in GO-2026-6611. Static reachability is present; concrete attack-path exploitability remains unverified.

**Disposition:** record an actionable infrastructure dependency observation and update the indirect `x/net` dependency to a compatible fixed release (the scan specifies `v0.60.0`), then rerun infrastructure checks and govulncheck. Do not claim “no reachable vulnerabilities,” “zero vulnerabilities,” or a clean scan across all modules. Do not promote this evidence alone to a demonstrated production FIX/FIXP exploit. The project boundary and unverified exploit preconditions belong beside the scan result, not in place of it. No dependency update or source edit was made during this audit triage.

Suggested report wording: “The Jepsen provisioning module has five symbol-reachable advisories in indirect `golang.org/x/net v0.59.0` (fixed in `v0.60.0`) and one module-only OpenPGP advisory. Their concrete exploitability in the Pulumi workflows was not established; the provisioning dependency needs maintenance. Production library scans are reported separately.”

Machine-readable counts: `infra-scan-summary.json` in this directory.
