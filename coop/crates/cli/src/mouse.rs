// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Mouse reports for an app that turned mouse tracking on.
//!
//! [`ModeScanner`] follows the DEC private modes the app sets on its output
//! (`CSI ? Pm h` / `CSI ? Pm l`): the mouse tracking it asked for (9, 1000,
//! 1002, 1003) and whether it wants SGR reports (1006). [`encode`] builds the
//! report the app expects for one press or release, so a caller of
//! `POST /api/v1/input/mouse` (the Glass) names a button and a cell and never
//! supplies raw bytes, and nothing is written while the app is not listening.

use serde::{Deserialize, Serialize};

/// The mouse modes the app has set.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct MouseModes {
    /// The tracking mode set last (9, 1000, 1002 or 1003); 0 for none.
    pub tracking: u16,
    /// SGR extended reports (mode 1006).
    pub sgr: bool,
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
enum State {
    #[default]
    Ground,
    Esc,
    Csi,
    Dec,
}

/// Up to this many parameters of one `CSI ? Pm h|l` are read; the rest are ignored.
const MAX_PARAMS: usize = 8;

/// Follows [`MouseModes`] through PTY output, across `feed()` boundaries.
#[derive(Debug, Clone, Default)]
pub struct ModeScanner {
    modes: MouseModes,
    state: State,
    params: [u16; MAX_PARAMS],
    len: usize,
    cur: u16,
}

impl ModeScanner {
    /// The modes as of the bytes scanned so far.
    pub fn modes(&self) -> MouseModes {
        self.modes
    }

    /// Scan PTY output for mode changes.
    pub fn scan(&mut self, data: &[u8]) {
        for &b in data {
            self.state = match (self.state, b) {
                (_, 0x1b) => State::Esc,
                (State::Esc, b'[') => State::Csi,
                (State::Esc, b'c') => {
                    // RIS: a full reset turns mouse reporting off.
                    self.modes = MouseModes::default();
                    State::Ground
                }
                (State::Csi, b'?') => {
                    self.len = 0;
                    self.cur = 0;
                    State::Dec
                }
                (State::Dec, b'0'..=b'9') => {
                    self.cur = self.cur.saturating_mul(10).saturating_add(u16::from(b - b'0'));
                    State::Dec
                }
                (State::Dec, b';') => {
                    self.push();
                    State::Dec
                }
                (State::Dec, b'h' | b'l') => {
                    self.push();
                    self.apply(b == b'h');
                    State::Ground
                }
                _ => State::Ground,
            };
        }
    }

    fn push(&mut self) {
        if self.len < MAX_PARAMS {
            self.params[self.len] = self.cur;
            self.len += 1;
        }
        self.cur = 0;
    }

    fn apply(&mut self, set: bool) {
        for &p in &self.params[..self.len] {
            match p {
                // As xterm.js does: setting one picks it, resetting any turns tracking off.
                9 | 1000 | 1002 | 1003 => self.modes.tracking = if set { p } else { 0 },
                1006 => self.modes.sgr = set,
                _ => {}
            }
        }
    }
}

/// A mouse button, as `POST /api/v1/input/mouse` names it.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MouseButton {
    Left,
    Middle,
    Right,
    WheelUp,
    WheelDown,
}

/// One press or release at a cell (1-based `col` and `row`).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct MouseEvent {
    pub button: MouseButton,
    pub col: u16,
    pub row: u16,
    #[serde(default)]
    pub release: bool,
    #[serde(default)]
    pub shift: bool,
    #[serde(default)]
    pub alt: bool,
    #[serde(default)]
    pub ctrl: bool,
}

/// The report for `ev` in the encoding `modes` asks for, or `None` when there
/// is nothing to send: tracking is off, the event has no report in this mode
/// (a wheel release; a release, wheel or modifier under X10 mode 9), or the
/// cell cannot be said in the default encoding (past 223).
pub fn encode(modes: MouseModes, ev: &MouseEvent) -> Option<Vec<u8>> {
    let wheel = matches!(ev.button, MouseButton::WheelUp | MouseButton::WheelDown);
    if modes.tracking == 0 || ev.col == 0 || ev.row == 0 || (ev.release && wheel) {
        return None;
    }
    if modes.tracking == 9 && (ev.release || wheel) {
        return None;
    }
    let mut cb: u8 = match ev.button {
        MouseButton::Left => 0,
        MouseButton::Middle => 1,
        MouseButton::Right => 2,
        MouseButton::WheelUp => 64,
        MouseButton::WheelDown => 65,
    };
    if modes.tracking != 9 {
        cb |= (u8::from(ev.shift) * 4) | (u8::from(ev.alt) * 8) | (u8::from(ev.ctrl) * 16);
    }
    if modes.sgr {
        let fin = if ev.release { 'm' } else { 'M' };
        return Some(format!("\x1b[<{cb};{};{}{fin}", ev.col, ev.row).into_bytes());
    }
    // Default encoding: ESC [ M and three bytes offset by 32; a release is button 3.
    if ev.col > 223 || ev.row > 223 {
        return None;
    }
    if ev.release {
        cb |= 3;
    }
    Some(vec![0x1b, b'[', b'M', 32 + cb, 32 + ev.col as u8, 32 + ev.row as u8])
}

#[cfg(test)]
#[path = "mouse_tests.rs"]
mod tests;
