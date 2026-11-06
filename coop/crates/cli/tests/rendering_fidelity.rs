// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Layer A: API-level rendering fidelity tests.
//!
//! Feed known byte sequences through the Store pipeline (Screen + RingBuffer),
//! compare results against a fresh `avt::Vt` oracle. No external dependencies
//! required — these are fast, isolated tests.

use coop::rendering_test_support::{
    assert_lines_eq, avt_ansi_from_bytes, avt_lines_from_bytes, feed_and_snapshot, normalize_lines,
};
use coop::screen::Screen;
use coop::test_support::{StoreBuilder, StoreCtx};

// -- Test 1: Plain text -------------------------------------------------------

#[tokio::test]
async fn screen_matches_fresh_avt_for_plain_text() -> anyhow::Result<()> {
    let text = b"The quick brown fox jumps over the lazy dog.\r\nLine two of plain text.\r\nLine three finishes here.";

    let (snap, _ring) = feed_and_snapshot(&[text.as_slice()], 80, 24, 65536).await;
    let oracle = avt_lines_from_bytes(text, 80, 24);

    assert_lines_eq("screen", &snap.lines, "avt_oracle", &oracle);

    Ok(())
}

// -- Test 2: ANSI colors ------------------------------------------------------

#[tokio::test]
async fn screen_matches_fresh_avt_for_ansi_colors() -> anyhow::Result<()> {
    let data = b"\x1b[31mRed text\x1b[0m and \x1b[32mgreen text\x1b[0m and \x1b[1;34mbold blue\x1b[0m done.";

    let (snap, _ring) = feed_and_snapshot(&[data.as_slice()], 80, 24, 65536).await;

    // Plain text comparison
    let oracle_text = avt_lines_from_bytes(data, 80, 24);
    assert_lines_eq("screen.lines", &snap.lines, "avt_oracle_text", &oracle_text);

    // ANSI comparison
    let oracle_ansi = avt_ansi_from_bytes(data, 80, 24);
    assert_lines_eq("screen.ansi", &snap.ansi, "avt_oracle_ansi", &oracle_ansi);

    Ok(())
}

// -- Test 3: Replay reconstituted matches screen ------------------------------

#[tokio::test]
async fn replay_reconstituted_matches_screen() -> anyhow::Result<()> {
    let data = b"Hello from the ring buffer!\r\nSecond line here.";

    let (snap, ring_bytes) = feed_and_snapshot(&[data.as_slice()], 80, 24, 65536).await;

    // Feed ring bytes into a fresh avt::Vt
    let reconstituted = avt_lines_from_bytes(&ring_bytes, 80, 24);

    assert_lines_eq("screen", &snap.lines, "ring_reconstituted", &reconstituted);

    Ok(())
}

// -- Test 4: Replay reconstituted after ring wrap-around ----------------------

#[tokio::test]
async fn replay_reconstituted_matches_screen_after_wrapping() -> anyhow::Result<()> {
    // Use a tiny ring (64 bytes) so the first chunk gets overwritten.
    let chunk1 = b"AAAAAAAAAA BBBBBBBBBB CCCCCCCCCC DDDDDDDDDD "; // 44 bytes
    let chunk2 = b"Final visible text after wrap."; // 30 bytes, total 74 > 64

    let (snap, ring_bytes) =
        feed_and_snapshot(&[chunk1.as_slice(), chunk2.as_slice()], 80, 24, 64).await;

    // The ring only retains the most recent 64 bytes. Feed those into a fresh
    // avt — the reconstituted screen won't match exactly (earlier data lost),
    // but the visible final content should be present in both.
    let reconstituted = avt_lines_from_bytes(&ring_bytes, 80, 24);

    // Both should contain the final text.
    let snap_text = snap.lines.join("\n");
    let recon_text = reconstituted.join("\n");
    assert!(
        recon_text.contains("Final visible text after wrap."),
        "ring reconstituted should contain final text, got: {recon_text:?}"
    );
    assert!(
        snap_text.contains("Final visible text after wrap."),
        "screen should contain final text, got: {snap_text:?}"
    );

    Ok(())
}

// -- Test 5: Multiline scrolling ----------------------------------------------

#[tokio::test]
async fn multiline_output_scrolling() -> anyhow::Result<()> {
    // 35 lines into a 24-row screen.
    let mut data = Vec::new();
    for i in 1..=35 {
        if i > 1 {
            data.extend_from_slice(b"\r\n");
        }
        data.extend_from_slice(format!("Line {:02}", i).as_bytes());
    }

    let (snap, _ring) = feed_and_snapshot(&[&data], 80, 24, 65536).await;

    // The screen should show the last 24 lines (12..35).
    let visible = normalize_lines(&snap.lines);
    assert!(!visible.is_empty(), "screen should have visible content");

    // Last visible line should be "Line 35".
    let last = visible.last().unwrap_or(&String::new()).clone();
    assert!(last.contains("Line 35"), "last line should be 'Line 35', got: {last:?}");

    // First visible line should be "Line 12" (35 - 24 + 1 = 12).
    let first = visible.first().unwrap_or(&String::new()).clone();
    assert!(first.contains("Line 12"), "first visible line should be 'Line 12', got: {first:?}");

    Ok(())
}

// -- Test 6: UTF-8 multibyte split across chunks -----------------------------

#[tokio::test]
async fn utf8_multibyte_through_pipeline() -> anyhow::Result<()> {
    // Split multi-byte chars across chunk boundaries.
    // café: c a f [0xC3 | 0xA9]  (split é across two chunks)
    let chunk1: &[u8] = b"caf\xC3";
    let chunk2: &[u8] = b"\xA9 na\xC3\xAFve";

    let (snap, ring_bytes) = feed_and_snapshot(&[chunk1, chunk2], 80, 24, 65536).await;

    let text = snap.lines.join("\n");
    assert!(text.contains("café naïve"), "expected 'café naïve', got: {text:?}");

    // Ring should have all bytes for reconstitution.
    let reconstituted = avt_lines_from_bytes(&ring_bytes, 80, 24);
    let recon_text = reconstituted.join("\n");
    assert!(
        recon_text.contains("café naïve"),
        "reconstituted should contain 'café naïve', got: {recon_text:?}"
    );

    Ok(())
}

// -- Test 7: ANSI 256 color preservation --------------------------------------

#[tokio::test]
async fn ansi_256_color_preservation() -> anyhow::Result<()> {
    // 256-color: \x1b[38;5;196m = bright red foreground
    let data = b"\x1b[38;5;196mBright red\x1b[0m normal";

    let (snap, _ring) = feed_and_snapshot(&[data.as_slice()], 80, 24, 65536).await;

    // Text content
    assert!(snap.lines[0].contains("Bright red"), "text: {:?}", snap.lines[0]);
    assert!(snap.lines[0].contains("normal"), "text: {:?}", snap.lines[0]);

    // ANSI should contain 256-color sequence
    let ansi = &snap.ansi[0];
    assert!(ansi.contains("38;5;196"), "ANSI output should contain 256-color code, got: {ansi:?}");

    Ok(())
}

// -- Test 8: ANSI RGB color preservation --------------------------------------

#[tokio::test]
async fn ansi_rgb_color_preservation() -> anyhow::Result<()> {
    // RGB color: \x1b[38;2;255;128;0m = orange foreground
    let data = b"\x1b[38;2;255;128;0mOrange\x1b[0m end";

    let (snap, _ring) = feed_and_snapshot(&[data.as_slice()], 80, 24, 65536).await;

    assert!(snap.lines[0].contains("Orange"), "text: {:?}", snap.lines[0]);

    let ansi = &snap.ansi[0];
    assert!(
        ansi.contains("38;2;255;128;0"),
        "ANSI output should contain RGB color code, got: {ansi:?}"
    );

    Ok(())
}

// -- Test 9: Bold, italic, underline preservation -----------------------------

#[tokio::test]
async fn bold_italic_underline_preservation() -> anyhow::Result<()> {
    let data = b"\x1b[1mBold\x1b[0m \x1b[3mItalic\x1b[0m \x1b[4mUnderline\x1b[0m";

    let (snap, _ring) = feed_and_snapshot(&[data.as_slice()], 80, 24, 65536).await;

    // Text content
    assert!(snap.lines[0].contains("Bold"), "text: {:?}", snap.lines[0]);
    assert!(snap.lines[0].contains("Italic"), "text: {:?}", snap.lines[0]);
    assert!(snap.lines[0].contains("Underline"), "text: {:?}", snap.lines[0]);

    // ANSI should contain attribute codes
    let ansi = &snap.ansi[0];
    assert!(ansi.contains(";1m"), "ANSI should contain bold (;1m), got: {ansi:?}");
    assert!(ansi.contains(";3m"), "ANSI should contain italic (;3m), got: {ansi:?}");
    assert!(ansi.contains(";4m"), "ANSI should contain underline (;4m), got: {ansi:?}");

    Ok(())
}

// -- Test 10: Cursor movement sequences ---------------------------------------

#[tokio::test]
async fn cursor_movement_sequences() -> anyhow::Result<()> {
    // Move cursor to row 5, col 10 (1-indexed), write text.
    let data = b"\x1b[5;10HPlaced here";

    let (snap, _ring) = feed_and_snapshot(&[data.as_slice()], 80, 24, 65536).await;

    // Row 4 (0-indexed) should contain "Placed here" starting at column 9.
    let line = &snap.lines[4];
    assert!(line.contains("Placed here"), "line 4: {line:?}");

    // Verify the text starts at the right position (9 spaces of padding).
    let trimmed = line.trim_start();
    assert_eq!(trimmed, "Placed here");
    let padding = line.len() - trimmed.len();
    assert_eq!(padding, 9, "expected 9 chars of padding, got {padding}");

    Ok(())
}

// -- Test 11: Alt screen toggle preserves primary -----------------------------

#[tokio::test]
async fn alt_screen_toggle_preserves_primary() -> anyhow::Result<()> {
    let mut screen = Screen::new(80, 24);

    // Write to primary screen
    screen.feed(b"Primary content");
    let snap1 = screen.snapshot();
    assert!(snap1.lines[0].contains("Primary content"));
    assert!(!snap1.alt_screen);

    // Enter alt screen
    screen.feed(b"\x1b[?1049h");
    assert!(screen.is_alt_screen());

    // Write to alt screen
    screen.feed(b"Alt screen text");
    let snap_alt = screen.snapshot();
    assert!(snap_alt.alt_screen);

    // Exit alt screen — primary content should be restored
    screen.feed(b"\x1b[?1049l");
    assert!(!screen.is_alt_screen());
    let snap2 = screen.snapshot();
    assert!(!snap2.alt_screen);
    assert!(
        snap2.lines[0].contains("Primary content"),
        "primary content should be preserved: {:?}",
        snap2.lines[0]
    );

    Ok(())
}

// -- Test 12: Ring replay reconstituted screen matches Store screen ----------

#[tokio::test]
async fn ws_replay_reconstituted_screen_matches_api_screen() -> anyhow::Result<()> {
    let data = b"WebSocket replay test\r\nWith multiple lines\r\nAnd \x1b[31mcolor\x1b[0m too";

    let StoreCtx { store, .. } = StoreBuilder::new().ring_size(65536).build();

    // Feed data into store's screen and ring (simulates the session output path).
    {
        let mut screen = store.terminal.screen.write().await;
        screen.feed(data);
        let mut ring = store.terminal.ring.write().await;
        ring.write(data);
    }

    // Read screen snapshot (what the HTTP API would return).
    let snap = store.terminal.screen.read().await.snapshot();

    // Read ring buffer contents (what WS replay would deliver).
    let ring = store.terminal.ring.read().await;
    let oldest = ring.oldest_offset();
    let replay_bytes = match ring.read_from(oldest) {
        Some((a, b)) => {
            let mut v = Vec::with_capacity(a.len() + b.len());
            v.extend_from_slice(a);
            v.extend_from_slice(b);
            v
        }
        None => Vec::new(),
    };

    // Feed replay bytes into fresh avt oracle.
    let oracle = avt_lines_from_bytes(&replay_bytes, 80, 24);

    assert_lines_eq("store_screen", &snap.lines, "ring_replay_oracle", &oracle);

    Ok(())
}
