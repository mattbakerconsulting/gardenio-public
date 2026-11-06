// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

fn scanned(chunks: &[&[u8]]) -> MouseModes {
    let mut s = ModeScanner::default();
    for c in chunks {
        s.scan(c);
    }
    s.modes()
}

fn press(button: MouseButton, col: u16, row: u16) -> MouseEvent {
    MouseEvent { button, col, row, release: false, shift: false, alt: false, ctrl: false }
}

#[test]
fn scanner_follows_tracking_and_sgr() {
    assert_eq!(scanned(&[b"plain text"]), MouseModes::default());
    // crossterm EnableMouseCapture: one mode per sequence.
    assert_eq!(
        scanned(&[b"\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1015h\x1b[?1006h"]),
        MouseModes { tracking: 1003, sgr: true }
    );
    // ncurses xterm-1006 XM: both in one sequence.
    assert_eq!(scanned(&[b"\x1b[?1006;1000h"]), MouseModes { tracking: 1000, sgr: true });
    assert_eq!(scanned(&[b"\x1b[?1006;1000h", b"\x1b[?1006;1000l"]), MouseModes::default());
    // Resetting any tracking mode turns tracking off, as xterm.js does.
    assert_eq!(
        scanned(&[b"\x1b[?1002h\x1b[?1006h", b"\x1b[?1000l"]),
        MouseModes { tracking: 0, sgr: true }
    );
    // RIS resets everything.
    assert_eq!(scanned(&[b"\x1b[?1000;1006h", b"\x1bc"]), MouseModes::default());
    // Other private modes and plain CSI sequences change nothing.
    assert_eq!(scanned(&[b"\x1b[?1049h\x1b[1000h\x1b[?25l"]), MouseModes::default());
}

#[test]
fn scanner_reads_sequences_split_across_feeds() {
    assert_eq!(
        scanned(&[b"ab\x1b", b"[?10", b"06;10", b"02", b"h"]),
        MouseModes { tracking: 1002, sgr: true }
    );
}

#[test]
fn encode_sgr() {
    let m = MouseModes { tracking: 1000, sgr: true };
    assert_eq!(encode(m, &press(MouseButton::Left, 10, 5)), Some(b"\x1b[<0;10;5M".to_vec()));
    let mut ev = press(MouseButton::Right, 300, 400);
    ev.release = true;
    ev.ctrl = true;
    assert_eq!(encode(m, &ev), Some(b"\x1b[<18;300;400m".to_vec()));
    let mut ev = press(MouseButton::WheelDown, 1, 1);
    ev.shift = true;
    assert_eq!(encode(m, &ev), Some(b"\x1b[<69;1;1M".to_vec()));
    ev.release = true;
    assert_eq!(encode(m, &ev), None);
}

#[test]
fn encode_default() {
    let m = MouseModes { tracking: 1002, sgr: false };
    assert_eq!(encode(m, &press(MouseButton::Middle, 1, 2)), Some(b"\x1b[M!!\"".to_vec()));
    let mut ev = press(MouseButton::Left, 1, 1);
    ev.release = true;
    assert_eq!(encode(m, &ev), Some(b"\x1b[M#!!".to_vec()));
    assert_eq!(encode(m, &press(MouseButton::Left, 224, 1)), None);
}

#[test]
fn encode_x10_reports_presses_only() {
    let m = MouseModes { tracking: 9, sgr: false };
    let mut ev = press(MouseButton::Left, 1, 1);
    ev.alt = true;
    assert_eq!(encode(m, &ev), Some(b"\x1b[M !!".to_vec()));
    ev.release = true;
    assert_eq!(encode(m, &ev), None);
    assert_eq!(encode(m, &press(MouseButton::WheelUp, 1, 1)), None);
}

#[test]
fn encode_nothing_while_tracking_is_off() {
    let ev = press(MouseButton::Left, 1, 1);
    assert_eq!(encode(MouseModes { tracking: 0, sgr: true }, &ev), None);
    assert_eq!(
        encode(MouseModes { tracking: 1000, sgr: true }, &press(MouseButton::Left, 0, 1)),
        None
    );
}
