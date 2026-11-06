// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use crate::driver::AgentState;
use bytes::Bytes;
use nix::sys::signal::Signal;
use serde::{Deserialize, Serialize};

/// Raw output from the terminal backend.
#[derive(Debug, Clone)]
pub enum OutputEvent {
    Raw { data: Bytes, offset: u64 },
}

/// Agent state transition with sequence number for ordering.
#[derive(Debug, Clone, Serialize)]
pub struct TransitionEvent {
    pub prev: AgentState,
    pub next: AgentState,
    pub seq: u64,
    pub cause: String,
    /// Snapshot of the last assistant message text at the time of this transition.
    pub last_message: Option<String>,
}

/// Input sent to the child process through the PTY.
#[derive(Debug)]
pub enum InputEvent {
    Write(Bytes),
    Resize {
        cols: u16,
        rows: u16,
    },
    Signal(PtySignal),
    /// Drain marker: notifies the sender (via oneshot) when all prior
    /// writes have been flushed to the PTY.  Used by `deliver_steps` to
    /// ensure the delay timer starts *after* the PTY write completes.
    WaitForDrain(tokio::sync::oneshot::Sender<()>),
}

/// A prompt response was delivered to the agent's terminal (auto-dismiss or API).
#[derive(Debug, Clone, Serialize)]
pub struct PromptOutcome {
    /// How the response was triggered: `"groom"` (auto-dismiss) or `"api"`.
    pub source: String,
    /// Prompt type responded to (e.g. `"setup"`, `"permission"`).
    pub r#type: String,
    /// Prompt subtype (e.g. `"login_success"`, `"trust"`).
    pub subtype: Option<String>,
    /// Option number selected (e.g. 1 for "Yes"), or `None` for Enter-only.
    pub option: Option<u32>,
}

/// Raw hook event JSON from the hook FIFO pipe.
#[derive(Debug, Clone, Serialize)]
pub struct RawHookEvent {
    pub json: serde_json::Value,
}

/// Raw agent message JSON from stdout (Tier 3) or log file (Tier 2).
#[derive(Debug, Clone)]
pub struct RawMessageEvent {
    pub json: serde_json::Value,
    /// Origin of the message: `"stdout"` (Tier 3) or `"log"` (Tier 2).
    pub source: String,
}

/// Ring capacity for the `message_tx` broadcast that fans raw agent messages out
/// to the NATS forwarder, the WebSocket live view, and the gRPC
/// `stream_raw_messages` stream (ka-0000006823).
///
/// Was 64. The producer (Tier 2 log detector, `driver/claude/stream.rs`) reads
/// *all* newly-appended JSONL lines in one `LogWatcher::read_new_lines` pass and
/// iterates that batch calling the synchronous `broadcast::send` per line. The
/// `.await` points inside that loop (an uncontended `RwLock` write, a non-full
/// bounded mpsc send) do not reliably yield under tokio's cooperative budget, so
/// a catch-up flurry — a resume from a far-behind `log_offset`, or several turns
/// buffered across a poll gap — can enqueue **more than 64 messages before any
/// consumer task is scheduled**. tokio broadcast then overwrites the oldest ring
/// slots, and the *next* `recv()` returns `RecvError::Lagged(n)`; the NATS
/// forwarder (`transport/nats_forward.rs`) drops those. A dropped
/// `assistant_text` is an unpublished plan turn — it never reaches JetStream, so
/// neither live delivery nor the full-tail replay (ka-0000007237) can recover it.
/// This is the pre-JetStream drop point.
///
/// The drop cause is that *tight-loop producer burst*, not a slow sibling
/// consumer: tokio broadcast receivers have independent cursors, so a stalled
/// WS/gRPC viewer can only lag itself, never the forwarder. That is why the fix
/// is a wider shared ring rather than a dedicated per-consumer channel (which
/// would add complexity for no benefit given independent cursors) or an unbounded
/// mpsc drain (which the forwarder's `await`-per-publish loop would let grow
/// without bound during a NATS outage — an OOM risk).
///
/// 2048 gives ~30x headroom over the observed 64 overflow while `RawMessageEvent`
/// is a small heap value (a `serde_json::Value` + a `String`), so the worst-case
/// transiently-retained ring is single-digit MB. Caveat: this *widens the margin,
/// it does not eliminate the mode* — a pathological catch-up burst larger than
/// the ring would still Lagged-drop. If that recurs, the fix is producer-side
/// batching/yielding (or a bounded catch-up cap), not an ever-larger ring.
///
/// The sibling `hook_tx` is left at 64: its producer (`driver/hook_detect.rs`)
/// is a `select!` loop that `.await`s `receiver.next_event()` once per iteration,
/// so consumers are scheduled *between* sends and the tight-loop batch burst that
/// motivates this bump does not apply there.
pub const MESSAGE_TX_CAPACITY: usize = 2048;

/// Profile lifecycle event emitted by the profile rotation system.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "event", rename_all = "snake_case")]
pub enum ProfileEvent {
    /// Active profile changed after a successful switch.
    #[serde(rename = "profile:switched")]
    ProfileSwitched { from: Option<String>, to: String },
    /// A single profile became rate-limited.
    #[serde(rename = "profile:exhausted")]
    ProfileExhausted { profile: String },
    /// All profiles are on cooldown — agent is parked.
    #[serde(rename = "profile:rotation:exhausted")]
    ProfileRotationExhausted { retry_after_secs: u64 },
}

/// Named signals that can be delivered to the child process.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PtySignal {
    Hup,
    Int,
    Quit,
    Kill,
    Usr1,
    Usr2,
    Term,
    Cont,
    Stop,
    Tstp,
    Winch,
}

impl PtySignal {
    /// Parse a signal name (e.g. "SIGINT", "INT", "2") into a `PtySignal`.
    pub fn from_name(name: &str) -> Option<Self> {
        let upper = name.to_uppercase();
        let bare: &str = match upper.strip_prefix("SIG") {
            Some(s) => s,
            None => &upper,
        };

        match bare {
            "HUP" | "1" => Some(Self::Hup),
            "INT" | "2" => Some(Self::Int),
            "QUIT" | "3" => Some(Self::Quit),
            "KILL" | "9" => Some(Self::Kill),
            "USR1" | "10" => Some(Self::Usr1),
            "USR2" | "12" => Some(Self::Usr2),
            "TERM" | "15" => Some(Self::Term),
            "CONT" | "18" => Some(Self::Cont),
            "STOP" | "19" => Some(Self::Stop),
            "TSTP" | "20" => Some(Self::Tstp),
            "WINCH" | "28" => Some(Self::Winch),
            _ => None,
        }
    }

    /// Convert to the corresponding `nix` signal for delivery.
    pub fn to_nix(self) -> Signal {
        match self {
            Self::Hup => Signal::SIGHUP,
            Self::Int => Signal::SIGINT,
            Self::Quit => Signal::SIGQUIT,
            Self::Kill => Signal::SIGKILL,
            Self::Usr1 => Signal::SIGUSR1,
            Self::Usr2 => Signal::SIGUSR2,
            Self::Term => Signal::SIGTERM,
            Self::Cont => Signal::SIGCONT,
            Self::Stop => Signal::SIGSTOP,
            Self::Tstp => Signal::SIGTSTP,
            Self::Winch => Signal::SIGWINCH,
        }
    }
}

#[cfg(test)]
#[path = "event_tests.rs"]
mod tests;
