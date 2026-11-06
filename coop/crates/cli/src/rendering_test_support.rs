// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Shared helpers for rendering fidelity tests.
//!
//! Provides utilities to feed bytes through the Store pipeline, create
//! independent avt oracle screens, and compare screen output with clear
//! diff-style error messages.

use crate::ring::RingBuffer;
use crate::screen::ScreenSnapshot;
use crate::test_support::{StoreBuilder, StoreCtx};

/// Feed byte chunks into a Store's screen and ring buffer, returning a snapshot
/// and the complete ring buffer contents.
///
/// Simulates the PTY output path: each chunk is written to both the screen
/// (via `feed()`) and the ring buffer (via `write()`).
pub async fn feed_and_snapshot(
    chunks: &[&[u8]],
    cols: u16,
    rows: u16,
    ring_size: usize,
) -> (ScreenSnapshot, Vec<u8>) {
    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(ring_size).build();

    // Resize to requested dimensions.
    {
        let mut screen = store.terminal.screen.write().await;
        screen.resize(cols, rows);
    }

    for chunk in chunks {
        let mut screen = store.terminal.screen.write().await;
        screen.feed(chunk);
        let mut ring = store.terminal.ring.write().await;
        ring.write(chunk);
    }

    let snap = store.terminal.screen.read().await.snapshot();
    let ring = store.terminal.ring.read().await;
    let ring_bytes = read_all_ring(&ring);

    (snap, ring_bytes)
}

/// Read all available bytes from a ring buffer (from oldest offset to current).
fn read_all_ring(ring: &RingBuffer) -> Vec<u8> {
    let oldest = ring.oldest_offset();
    match ring.read_from(oldest) {
        Some((a, b)) => {
            let mut out = Vec::with_capacity(a.len() + b.len());
            out.extend_from_slice(a);
            out.extend_from_slice(b);
            out
        }
        None => Vec::new(),
    }
}

/// Create a fresh avt::Vt, feed the given bytes, and return the text lines.
///
/// This acts as an independent oracle — the bytes go through a standalone
/// virtual terminal, not through coop's Screen wrapper.
pub fn avt_lines_from_bytes(data: &[u8], cols: u16, rows: u16) -> Vec<String> {
    let mut vt = avt::Vt::new(cols as usize, rows as usize);
    let s = String::from_utf8_lossy(data);
    let _ = vt.feed_str(&s);
    vt.view().map(|line| line.text().trim_end().to_owned()).collect()
}

/// Create a fresh avt::Vt, feed the given bytes, and return ANSI-escaped lines.
///
/// Mirrors `screen.rs`'s `line_to_ansi` logic for oracle comparison.
pub fn avt_ansi_from_bytes(data: &[u8], cols: u16, rows: u16) -> Vec<String> {
    let mut vt = avt::Vt::new(cols as usize, rows as usize);
    let s = String::from_utf8_lossy(data);
    let _ = vt.feed_str(&s);
    vt.view().map(line_to_ansi).collect()
}

/// Normalize lines for comparison: trim trailing whitespace per-line, then
/// strip trailing empty lines from the end.
pub fn normalize_lines(lines: &[String]) -> Vec<String> {
    let trimmed: Vec<String> = lines.iter().map(|l| l.trim_end().to_owned()).collect();
    let last_non_empty = trimmed.iter().rposition(|l| !l.is_empty()).map(|i| i + 1).unwrap_or(0);
    trimmed[..last_non_empty].to_vec()
}

/// Assert that two sets of lines are equal, with a clear diff-style error
/// message on failure.
#[allow(clippy::panic)]
pub fn assert_lines_eq(label_a: &str, lines_a: &[String], label_b: &str, lines_b: &[String]) {
    let norm_a = normalize_lines(lines_a);
    let norm_b = normalize_lines(lines_b);

    if norm_a == norm_b {
        return;
    }

    let max_len = norm_a.len().max(norm_b.len());
    let mut diff = String::new();
    diff.push_str(&format!(
        "\n--- {label_a} ({} lines)\n+++ {label_b} ({} lines)\n",
        norm_a.len(),
        norm_b.len()
    ));

    for i in 0..max_len {
        let a = norm_a.get(i).map(|s| s.as_str()).unwrap_or("<missing>");
        let b = norm_b.get(i).map(|s| s.as_str()).unwrap_or("<missing>");
        if a != b {
            diff.push_str(&format!("  line {i}: {label_a}={a:?}\n"));
            diff.push_str(&format!("  line {i}: {label_b}={b:?}\n"));
        }
    }

    panic!("lines differ:{diff}");
}

/// Strip attach-specific framing sequences from raw bytes.
///
/// Removes: SMCUP/RMCUP (alt screen), DEC 2026 synchronized updates,
/// DECSTBM scroll region, clear/home, and statusline cursor save/restore.
pub fn strip_attach_framing(data: &[u8]) -> Vec<u8> {
    let sequences: &[&[u8]] = &[
        b"\x1b[?1049h",   // SMCUP
        b"\x1b[?1049l",   // RMCUP
        b"\x1b[?2026h",   // SYNC_START
        b"\x1b[?2026l",   // SYNC_END
        b"\x1b[2J\x1b[H", // CLEAR_HOME
    ];

    let mut out = data.to_vec();
    for seq in sequences {
        out = remove_sequence(&out, seq);
    }

    // Strip DECSTBM scroll region: \x1b[<n>r or \x1b[1;<n>r
    out = strip_decstbm(&out);

    out
}

/// Remove all occurrences of a byte sequence from data.
fn remove_sequence(data: &[u8], seq: &[u8]) -> Vec<u8> {
    if seq.is_empty() || data.len() < seq.len() {
        return data.to_vec();
    }
    let mut out = Vec::with_capacity(data.len());
    let mut i = 0;
    while i < data.len() {
        if i + seq.len() <= data.len() && &data[i..i + seq.len()] == seq {
            i += seq.len();
        } else {
            out.push(data[i]);
            i += 1;
        }
    }
    out
}

/// Strip DECSTBM scroll region sequences: \x1b[<digits>r and \x1b[<digits>;<digits>r
fn strip_decstbm(data: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(data.len());
    let mut i = 0;
    while i < data.len() {
        if i + 2 < data.len() && data[i] == 0x1b && data[i + 1] == b'[' {
            // Try to parse \x1b[<digits>[;<digits>]r
            let start = i;
            let mut j = i + 2;
            let mut found_r = false;
            while j < data.len() {
                if data[j].is_ascii_digit() || data[j] == b';' {
                    j += 1;
                } else if data[j] == b'r' {
                    found_r = true;
                    j += 1;
                    break;
                } else {
                    break;
                }
            }
            if found_r && j > start + 2 {
                // Skip this DECSTBM sequence
                i = j;
                continue;
            }
        }
        out.push(data[i]);
        i += 1;
    }
    out
}

// -- ANSI SGR generation (mirrors screen.rs for oracle) -----------------------

/// Encode a single avt color as SGR parameter(s).
fn color_sgr(c: &avt::Color, base: u8, out: &mut String) {
    use std::fmt::Write;
    match c {
        avt::Color::Indexed(n) if *n < 8 => {
            let _ = write!(out, ";{}", base + n);
        }
        avt::Color::Indexed(n) if *n < 16 => {
            let _ = write!(out, ";{}", base + 52 + n);
        }
        avt::Color::Indexed(n) => {
            let _ = write!(out, ";{};5;{}", base + 8, n);
        }
        avt::Color::RGB(rgb) => {
            let _ = write!(out, ";{};2;{};{};{}", base + 8, rgb.r, rgb.g, rgb.b);
        }
    }
}

/// Emit a full SGR reset-and-set sequence for `pen`.
fn pen_to_sgr(pen: &avt::Pen, out: &mut String) {
    out.push_str("\x1b[0");
    if let Some(c) = pen.foreground() {
        color_sgr(&c, 30, out);
    }
    if let Some(c) = pen.background() {
        color_sgr(&c, 40, out);
    }
    if pen.is_bold() {
        out.push_str(";1");
    }
    if pen.is_faint() {
        out.push_str(";2");
    }
    if pen.is_italic() {
        out.push_str(";3");
    }
    if pen.is_underline() {
        out.push_str(";4");
    }
    if pen.is_blink() {
        out.push_str(";5");
    }
    if pen.is_inverse() {
        out.push_str(";7");
    }
    if pen.is_strikethrough() {
        out.push_str(";9");
    }
    out.push('m');
}

/// Convert an avt Line to a string with ANSI SGR escapes.
fn line_to_ansi(line: &avt::Line) -> String {
    let mut s = String::new();
    let mut styled = false;

    for cells in line.chunks(|c1, c2| c1.pen() != c2.pen()) {
        let pen = cells[0].pen();
        if pen.is_default() {
            if styled {
                s.push_str("\x1b[0m");
                styled = false;
            }
        } else {
            pen_to_sgr(pen, &mut s);
            styled = true;
        }
        for cell in &cells {
            s.push(cell.char());
        }
    }

    if styled {
        s.push_str("\x1b[0m");
    }
    let trimmed_len = s.trim_end().len();
    s.truncate(trimmed_len);
    s
}
