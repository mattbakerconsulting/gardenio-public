# thawepoch — the thaw-epoch signal (Stage 0)

Foundation of the agent rehydration design
([`gardenio/docs/gvisor-runsc-cr/rehydration-design.md`](../../../docs/gvisor-runsc-cr/rehydration-design.md),
bead ka-0000001515; build bead ka-0000003040; epic ka-0000006959).

When a checkpointed gVisor agent is thawed into a fresh kubelet-owned pod, the
process never restarts — `main()` does not run again. Every credential and TCP
4-tuple the agent cached in RAM before the freeze is now stale or dead. This
package is the **signal** that tells the running agent "you were just thawed;
re-hydrate before you resume issuing work," and the **gate** the controller uses
to refuse to release the agent until it has.

Stage 0 deliberately ships **only the signal mechanics**. The actual rehydration
work plugs in as `Handler`s in the downstream build beads:

- **credential re-mint** — ka-0000007211 (Stage 1+2)
- **NATS reconnect** — ka-0000001901 (Stage 3+4, coop side)

## The on-disk contract

Everything keys off one state directory (the agent's coop state dir):

| File | Writer | Reader |
|---|---|---|
| `thaw-epoch.json` | controller thaw verb, post-restore | every consumer's `Watcher` |
| `thaw-epoch.gb.done` | gb supervisor `Watcher` | controller `WaitForAcks` |
| `thaw-epoch.coop.done` | coop `ThawWatcher` | controller `WaitForAcks` |

The **marker** carries a monotonic `epoch` plus the identity of the pod the agent
was thawed into (`pod_uid`, `node`) and optional hints the rehydration stages
consume (`frozen_at`, `deadline_ms`). All writes are atomic
(temp-file + rename) so a watcher never observes a partial file.

The marker/ack JSON is **byte-compatible across Go and Rust**: the Go
`thawepoch` package (gb supervisor + controller) and the coop `thaw` module
(`coop/crates/cli/src/thaw.rs`) read and write the same files. `ts` is treated as
an opaque string on both sides so coop needs no date crate.

## The three roles

1. **Controller (Go, `Write` + `WaitForAcks`)** — after `runsc restore`
   succeeds, calls `NextEpoch` + `Write` to drop the marker, then `WaitForAcks`
   to block until *every required consumer* (`gb`, `coop`) writes an OK ack for
   that epoch, or the deadline (default 10s) expires. On timeout or a consumer
   failure it **fails closed** (`GateResult.Released == false`): the agent is not
   marked live — no half-rehydrated agent running under ambiguous identity (C5).

2. **Agent consumers (`Watcher` in Go, `ThawWatcher` in Rust)** — each runs a
   poll loop (the safe floor on gVisor; an inotify front-end gives an eager
   `Notify`/`notify`-crate wake) that latches the epoch, runs its `Handler` once
   per increment, and writes its ack. Handlers **must be idempotent** — a
   poll+inotify double-fire or the drift backstop may invoke them twice. A
   fail-closed handler does **not** latch the epoch, so a corrected marker (or a
   controller retry) re-triggers it.

3. **Drift backstop (`DriftDetector`, Go)** — option (C) of the design. If the
   observed pod identity changes with no fresh marker (controller bug / manual
   restore), the watcher self-triggers so an un-marked thaw still eventually
   rehydrates rather than silently rotting. Primary path is the marker; this is
   the safety net.

## Wiring a rehydration stage (downstream beads)

Go (gb supervisor):

```go
w := &thawepoch.Watcher{
    StateDir: coopStateDir,
    Consumer: thawepoch.ConsumerGB,
    Handler: func(ctx context.Context, m thawepoch.Marker) (thawepoch.Ack, error) {
        // Stage 1+2: poison AWS/beads cred caches, re-read token files, ...
        return thawepoch.Ack{OK: true, Stages: map[string]string{"creds": "ok"}}, nil
    },
}
go w.Run(ctx)
```

Rust (coop):

```rust
struct ReconnectHandler { /* transport handles */ }
impl thaw::RehydrateHandler for ReconnectHandler {
    async fn rehydrate(&self, m: &thaw::Marker) -> thaw::Outcome {
        // Stage 3+4: reconnect_all(token) + eager announce + bridge re-dial
        thaw::Outcome::ok()
    }
}
thaw::ThawWatcher::new(state_dir, ReconnectHandler { .. }).run(shutdown).await;
```

Until those handlers land, the Stage-0 default (`AckOnlyHandler` / a nil Go
handler) simply acks OK on a present marker so the gate path is exercisable end
to end.
```
