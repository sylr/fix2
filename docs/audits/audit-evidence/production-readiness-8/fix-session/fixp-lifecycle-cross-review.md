# Independent source cross-review: FIXP callback serialization

Reviewed HEAD `c7e5579`, `fixp/session/session.go`, `server.go`, `handler.go`, `client.go`, `finish.go`, lifecycle tests and the application's documented handler model. This note is an independent source assessment; the codec reviewer owns the deterministic reproduction.

## Causal chain

`fixp/session/handler.go:28` promises callbacks run one at a time per session and in order. `docs/fixp-app.md:131` repeats that contract and recommends keeping reusable decoded messages per session because callbacks are serialized.

At the end of a connection, `Session.run` joins its writer and closes its transport (`session.go:590–591`), but then clears `s.conn` and publishes `StateUnbound` (`:595–597`) before invoking `OnUnbound` (`:605`). It closes that connection's `unbound` channel only after the callback returns (`:606`). No separate callback lock or in-flight lifecycle field was found.

Two supported paths bypass the remaining callback:

1. **Administrative retirement.** `Server.retireSession` takes `s.conn` under the session lock (`server.go:226`). If OnUnbound is already executing, it sees nil. It therefore skips the `c.unbound` join at `:251–254`, retires/syncs/closes the store and calls `OnFinalized` at `:273`, while OnUnbound can still be active. This does not require a new network connection.
2. **Re-establishment.** The server starts each accepted connection's `serve` in its own goroutine (`server.go:360–361`). After normal authentication and owner checking (`:588–603`), a new Establish claims `StateUnbound` (`:613–617`), writes its acknowledgment and returns to `serve` (`:664–668`). `serve` calls `s.run` (`:438`), which calls `OnEstablished` (`session.go:579`) without waiting for the prior connection's `c.unbound`. New `OnMessage` callbacks can follow on that new reader while the old OnUnbound remains active.

The second path is server-side. The ordinary library Client.Run loop calls connect/run synchronously (`client.go:103`, `:183`) and does not start its own next connection until its own OnUnbound returns. That sequential client behavior does not protect the server's callback: the client and server have different handlers, and the client can reconnect while the server is still cleaning up. A raw authenticated peer can make the ordering especially explicit. Authentication does gate the re-establishment path; this is not an unauthenticated session takeover claim.

## Safeguards that do not close the window

The session mutex protects internal state transitions, but is released before callback invocation. StateEstablishing prevents two new Establish claims; it does not fence the old OnUnbound. A currently bound retirement joins c.unbound correctly, but clearing s.conn too early makes that guard unavailable. An ordinary fully finished connection uses StateFinalizing, preventing re-establishment; external Retire can still see nil while its OnUnbound is active. The ordinary automatic-retirement timer runs after onUnbound returns, so it is not the main trigger; public Retire is.

Callbacks must return promptly. That requirement limits expected duration and does not permit overlap: even a short callback has an execution interval after the unbound state is published. A gated reproduction widens a real scheduling window. It should release/join the gate and avoid implying that an indefinitely blocked user callback is required in production.

## Impact and severity

**P2 is justified if the diagnostic confirms the public paths above.** This is a core handler safety contract, not just a late notification. Applications can reasonably keep per-session mutable state and resources without their own callback mutex. Concurrent unbind cleanup and establishment/finalization can race maps, overwrite connection-specific state, close newly initialized resources, or deliver new messages during old cleanup. The library's own code may remain free of data races because its state has locks; that does not make the promised callback execution safe for compliant applications.

Keep the claim bounded: source review alone does not establish durable message loss, process corruption, a crash in a shipping example or actual handler data races. A reproduction detecting active-callback overlap with atomics is sufficient to prove the contract violation without intentionally causing a user-memory race. P3 would be appropriate only if maintainers explicitly weaken the contract and prove all supported handler patterns tolerate overlap; current API/docs promise the opposite.

Acceptance should fence callback completion across connection generations and retirement. Preserve exclusivity through OnUnbound, then allow establishment/finalization, or serialize callback delivery separately. Cover both external Retire and re-establishment with a running OnUnbound; also verify no new OnMessage arrives early, callbacks complete in order, public state/Send behavior during unbinding remains coherent, and shutdown/reentrant callback operations do not acquire a new deadlock. Do not simply hold the existing session mutex around callbacks, since callbacks call back into Session.
