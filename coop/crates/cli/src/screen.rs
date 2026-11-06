// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::{Arc, OnceLock};

use serde::{Deserialize, Serialize};

/// Opaque terminal screen backed by an avt virtual terminal.
pub struct Screen {
    vt: avt::Vt,
    seq: u64,
    changed: bool,
    alt_screen: bool,
    /// The mouse modes the app has set, for `POST /api/v1/input/mouse`.
    mouse: crate::mouse::ModeScanner,
    /// Buffer for incomplete UTF-8 trailing bytes between `feed()` calls.
    utf8_buf: [u8; 3],
    utf8_buf_len: u8,
    /// Buffer for trailing bytes that may form an incomplete escape sequence
    /// across `feed()` calls (max sequence length is 8: `\x1b[?1049h`).
    esc_buf: [u8; 7],
    esc_buf_len: u8,
    /// Text-only snapshot shared by all consumers of the current sequence.
    text_cache: OnceLock<Arc<ScreenTextSnapshot>>,
}

impl std::fmt::Debug for Screen {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Screen")
            .field("seq", &self.seq)
            .field("changed", &self.changed)
            .field("alt_screen", &self.alt_screen)
            .finish()
    }
}

/// DECSET alternate screen buffer enable.
const ALT_SCREEN_ON: &[u8] = b"\x1b[?1049h";
/// DECRST alternate screen buffer disable.
const ALT_SCREEN_OFF: &[u8] = b"\x1b[?1049l";

/// Scan `data` for alt screen on/off sequences, updating `alt_screen`.
fn scan_alt_screen(data: &[u8], alt_screen: &mut bool) {
    if data.len() < ALT_SCREEN_ON.len() {
        return;
    }
    if data.windows(ALT_SCREEN_ON.len()).any(|w| w == ALT_SCREEN_ON) {
        *alt_screen = true;
    }
    if data.windows(ALT_SCREEN_OFF.len()).any(|w| w == ALT_SCREEN_OFF) {
        *alt_screen = false;
    }
}

/// Returns the number of trailing bytes that form an incomplete UTF-8 sequence.
///
/// Scans backwards from the end of `data` looking for a leading byte whose
/// expected sequence length exceeds the bytes available.  Returns 0 when the
/// tail is complete (or pure ASCII).
fn incomplete_utf8_tail_len(data: &[u8]) -> usize {
    let len = data.len();
    for i in 1..=len.min(3) {
        let byte = data[len - i];
        if byte < 0x80 {
            // ASCII — no incomplete sequence possible.
            return 0;
        }
        if byte >= 0xC0 {
            // Leading byte: check whether the sequence is complete.
            let expected = if byte < 0xE0 {
                2
            } else if byte < 0xF0 {
                3
            } else {
                4
            };
            return if i < expected { i } else { 0 };
        }
        // Continuation byte (0x80..0xBF) — keep scanning backwards.
    }
    // Only continuation bytes found with no leading byte — not a valid partial
    // sequence, let lossy handle it.
    0
}

impl Screen {
    /// Create a new screen with the given dimensions.
    pub fn new(cols: u16, rows: u16) -> Self {
        Self {
            vt: avt::Vt::new(cols as usize, rows as usize),
            seq: 0,
            changed: false,
            alt_screen: false,
            mouse: crate::mouse::ModeScanner::default(),
            utf8_buf: [0; 3],
            utf8_buf_len: 0,
            esc_buf: [0; 7],
            esc_buf_len: 0,
            text_cache: OnceLock::new(),
        }
    }

    /// Feed raw bytes from the PTY into the virtual terminal.
    pub fn feed(&mut self, data: &[u8]) {
        if data.is_empty() {
            return;
        }
        let _ = self.text_cache.take();

        // Prepend any buffered incomplete UTF-8 bytes from the previous call.
        let buf_len = self.utf8_buf_len as usize;
        let owned: Vec<u8>;
        let input = if buf_len == 0 {
            data
        } else {
            owned = [&self.utf8_buf[..buf_len], data].concat();
            self.utf8_buf_len = 0;
            &owned
        };

        // Track alt screen transitions from raw escape sequences since
        // avt::Vt doesn't expose the active buffer type.
        //
        // To detect sequences split across PTY read boundaries, we
        // prepend the esc_buf tail from the previous call to the start
        // of input, scan that combined region, then also scan the full
        // input.  Finally, buffer the last 7 bytes for next time.
        let esc_len = self.esc_buf_len as usize;
        if esc_len > 0 {
            // Build a small region: [esc_buf tail | first bytes of input]
            // that is large enough to complete any split sequence.
            let take = input.len().min(ALT_SCREEN_ON.len());
            let mut bridge = [0u8; 15]; // 7 + 8
            bridge[..esc_len].copy_from_slice(&self.esc_buf[..esc_len]);
            bridge[esc_len..esc_len + take].copy_from_slice(&input[..take]);
            let region = &bridge[..esc_len + take];
            scan_alt_screen(region, &mut self.alt_screen);
        }
        scan_alt_screen(input, &mut self.alt_screen);
        self.mouse.scan(data);

        // Buffer the last 7 bytes for cross-boundary detection next call.
        let tail_len = input.len().min(7);
        self.esc_buf[..tail_len].copy_from_slice(&input[input.len() - tail_len..]);
        self.esc_buf_len = tail_len as u8;

        // Split off any incomplete UTF-8 trailing bytes to buffer for next call.
        let tail = incomplete_utf8_tail_len(input);
        let (to_feed, to_buffer) = input.split_at(input.len() - tail);

        if !to_buffer.is_empty() {
            self.utf8_buf[..to_buffer.len()].copy_from_slice(to_buffer);
            self.utf8_buf_len = to_buffer.len() as u8;
        }

        if !to_feed.is_empty() {
            let s = String::from_utf8_lossy(to_feed);
            let _ = self.vt.feed_str(&s);
        }

        self.seq += 1;
        self.changed = true;
    }

    /// Capture a point-in-time snapshot of the screen contents.
    pub fn snapshot(&self) -> ScreenSnapshot {
        self.snapshot_with_ansi()
    }

    /// Capture and cache plain text lines and screen metadata without rendering ANSI.
    pub fn snapshot_text(&self) -> Arc<ScreenTextSnapshot> {
        Arc::clone(self.text_cache.get_or_init(|| {
            let metadata = self.metadata();
            Arc::new(ScreenTextSnapshot {
                lines: self.text_lines(),
                cols: metadata.cols,
                rows: metadata.rows,
                alt_screen: metadata.alt_screen,
                cursor: metadata.cursor,
                sequence: metadata.sequence,
            })
        }))
    }

    /// Capture plain text, ANSI-rendered lines, and screen metadata.
    pub fn snapshot_with_ansi(&self) -> ScreenSnapshot {
        let text = self.snapshot_text();
        let ansi: Vec<String> = self.vt.view().map(line_to_ansi).collect();

        ScreenSnapshot {
            lines: text.lines.clone(),
            ansi,
            cols: text.cols,
            rows: text.rows,
            alt_screen: text.alt_screen,
            cursor: text.cursor,
            sequence: text.sequence,
        }
    }

    /// Capture screen metadata without rendering text or ANSI lines.
    pub fn metadata(&self) -> ScreenMetadata {
        let (cols, rows) = self.vt.size();
        let cursor = self.vt.cursor();

        ScreenMetadata {
            cols: cols as u16,
            rows: rows as u16,
            alt_screen: self.alt_screen,
            cursor: CursorPosition { row: cursor.row as u16, col: cursor.col as u16 },
            sequence: self.seq,
        }
    }

    fn text_lines(&self) -> Vec<String> {
        self.vt.view().map(line_to_text).collect()
    }

    /// Whether the terminal is in alt screen mode.
    pub fn is_alt_screen(&self) -> bool {
        self.alt_screen
    }

    /// The mouse modes the app has set.
    pub fn mouse_modes(&self) -> crate::mouse::MouseModes {
        self.mouse.modes()
    }

    /// Whether the screen has been updated since the last `clear_changed`.
    pub fn changed(&self) -> bool {
        self.changed
    }

    /// Clear the changed flag.
    pub fn clear_changed(&mut self) {
        self.changed = false;
    }

    /// Current sequence number, incremented when feed or resize changes the screen.
    pub fn seq(&self) -> u64 {
        self.seq
    }

    /// Resize the virtual terminal.
    pub fn resize(&mut self, cols: u16, rows: u16) {
        let _ = self.text_cache.take();
        let _ = self.vt.resize(cols as usize, rows as usize);
        self.seq += 1;
        self.changed = true;
    }
}

/// Point-in-time capture of terminal text lines and metadata.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ScreenTextSnapshot {
    pub lines: Vec<String>,
    pub cols: u16,
    pub rows: u16,
    pub alt_screen: bool,
    pub cursor: CursorPosition,
    pub sequence: u64,
}

impl ScreenTextSnapshot {
    /// Return an empty snapshot used when the screen lock is unavailable.
    pub fn empty() -> Self {
        Self {
            lines: vec![],
            cols: 0,
            rows: 0,
            alt_screen: false,
            cursor: CursorPosition { row: 0, col: 0 },
            sequence: 0,
        }
    }
}

/// Point-in-time capture of terminal metadata without line contents.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct ScreenMetadata {
    pub cols: u16,
    pub rows: u16,
    pub alt_screen: bool,
    pub cursor: CursorPosition,
    pub sequence: u64,
}

/// Point-in-time capture of the terminal screen contents.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ScreenSnapshot {
    pub lines: Vec<String>,
    /// Lines with ANSI SGR escape sequences preserving colors and attributes.
    pub ansi: Vec<String>,
    pub cols: u16,
    pub rows: u16,
    pub alt_screen: bool,
    pub cursor: CursorPosition,
    pub sequence: u64,
}

/// Row and column position of the terminal cursor.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct CursorPosition {
    pub row: u16,
    pub col: u16,
}

// -- ANSI SGR generation from avt cells ---------------------------------------

/// Encode a single avt color as SGR parameter(s).
///
/// `base` is 30 for foreground, 40 for background.
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

/// Convert an avt [`Line`](avt::Line) to plain text.
fn line_to_text(line: &avt::Line) -> String {
    let text = line.text();
    text.trim_end().to_owned()
}

/// Convert an avt [`Line`](avt::Line) to a string with ANSI SGR escapes.
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

#[cfg(test)]
#[path = "screen_tests.rs"]
mod tests;
