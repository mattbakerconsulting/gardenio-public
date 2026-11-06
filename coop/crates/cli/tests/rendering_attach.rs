// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Layer C: Full E2E attach pipeline rendering tests.
//!
//! Starts coop with claudeless, then runs `coop attach --no-statusline` inside
//! a tmux session. Captures tmux output (which shows what attach rendered) and
//! compares against coop's server-side screen API.
//!
//! Requires `claudeless`, `tmux`, and a built `coop` binary in PATH or
//! target/debug.

mod rendering_support;

use std::time::Duration;

use rendering_support::{
    compare_output_region, normalize_lines, require_claudeless, require_tmux, CoopScenario,
    TmuxOracle,
};

const COLS: u16 = 80;
const ROWS: u16 = 24;
const WAIT: Duration = Duration::from_secs(30);

/// Find the coop binary path for use in tmux commands.
fn coop_bin() -> String {
    if let Ok(path) = std::env::var("COOP_BIN") {
        return path;
    }
    let manifest_dir = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    let target =
        manifest_dir.parent().and_then(|p| p.parent()).map(|p| p.join("target/debug/coop"));
    if let Some(path) = target {
        if path.exists() {
            return path.display().to_string();
        }
    }
    "coop".to_owned()
}

// -- Test 1: Attach screen matches server (plain) -----------------------------

#[test]
fn attach_screen_matches_server_plain() -> anyhow::Result<()> {
    require_claudeless();
    require_tmux();

    let sentinel = "The quick brown fox";
    let scenario = "render_plain.toml";

    // Start coop server.
    let coop = CoopScenario::start(scenario, "render plain", COLS, ROWS)?;
    coop.wait_for_screen_text(sentinel, WAIT)?;

    // Start attach inside tmux.
    let attach_cmd = format!("{} attach --no-statusline {}", coop_bin(), coop.base_url());
    let tmux = TmuxOracle::new(&attach_cmd, COLS, ROWS)?;
    let attach_lines = tmux.wait_for_text(sentinel, WAIT)?;

    // Compare attach output vs server screen API.
    let server_lines = coop.fetch_screen_lines()?;

    compare_output_region("server", &server_lines, "attach", &attach_lines, sentinel)?;

    Ok(())
}

// -- Test 2: Attach preserves colors ------------------------------------------

#[test]
fn attach_screen_matches_server_colors() -> anyhow::Result<()> {
    require_claudeless();
    require_tmux();

    let sentinel = "Red text";
    let scenario = "render_colors.toml";

    let coop = CoopScenario::start(scenario, "render colors", COLS, ROWS)?;
    coop.wait_for_screen_text(sentinel, WAIT)?;

    let attach_cmd = format!("{} attach --no-statusline {}", coop_bin(), coop.base_url());
    let tmux = TmuxOracle::new(&attach_cmd, COLS, ROWS)?;
    let attach_lines = tmux.wait_for_text(sentinel, WAIT)?;

    let server_lines = coop.fetch_screen_lines()?;

    // Compare text content (tmux capture-pane -p strips ANSI).
    compare_output_region("server", &server_lines, "attach", &attach_lines, sentinel)?;

    Ok(())
}

// -- Test 3: Attach preserves UTF-8 -------------------------------------------

#[test]
fn attach_screen_matches_server_utf8() -> anyhow::Result<()> {
    require_claudeless();
    require_tmux();

    let sentinel = "Unicode:";
    let scenario = "render_utf8.toml";

    let coop = CoopScenario::start(scenario, "render utf8", COLS, ROWS)?;
    coop.wait_for_screen_text(sentinel, WAIT)?;

    let attach_cmd = format!("{} attach --no-statusline {}", coop_bin(), coop.base_url());
    let tmux = TmuxOracle::new(&attach_cmd, COLS, ROWS)?;
    let attach_lines = tmux.wait_for_text(sentinel, WAIT)?;

    let server_lines = coop.fetch_screen_lines()?;

    compare_output_region("server", &server_lines, "attach", &attach_lines, sentinel)?;

    Ok(())
}

// -- Test 4: Statusline preserves content area --------------------------------

#[test]
fn attach_with_statusline_preserves_content() -> anyhow::Result<()> {
    require_claudeless();
    require_tmux();

    let sentinel = "The quick brown fox";
    let scenario = "render_plain.toml";

    // Start coop server with extra rows to accommodate statusline.
    let coop = CoopScenario::start(scenario, "render plain", COLS, ROWS + 1)?;
    coop.wait_for_screen_text(sentinel, WAIT)?;

    // Attach WITH statusline (default — no --no-statusline flag).
    let attach_cmd = format!("{} attach {}", coop_bin(), coop.base_url());
    let tmux = TmuxOracle::new(&attach_cmd, COLS, ROWS + 1)?;
    let attach_lines = tmux.wait_for_text(sentinel, WAIT)?;

    // The content area should still contain the sentinel text.
    let norm = normalize_lines(&attach_lines);
    let has_sentinel = norm.iter().any(|l| l.contains(sentinel));
    assert!(has_sentinel, "content area should contain sentinel: {norm:?}");

    // There should be at least ROWS+1 total lines (content + statusline).
    assert!(
        attach_lines.len() >= ROWS as usize,
        "should have at least {ROWS} lines, got {}",
        attach_lines.len()
    );

    Ok(())
}

// -- Test 5: Late-connect replay works ----------------------------------------

#[test]
fn attach_replay_after_late_connect() -> anyhow::Result<()> {
    require_claudeless();
    require_tmux();

    let sentinel = "The quick brown fox";
    let scenario = "render_plain.toml";

    // Start coop and wait for output to complete.
    let coop = CoopScenario::start(scenario, "render plain", COLS, ROWS)?;
    coop.wait_for_screen_text(sentinel, WAIT)?;

    // Brief pause to ensure all output is written.
    std::thread::sleep(Duration::from_millis(500));

    // NOW connect attach — it should replay the buffered output.
    let attach_cmd = format!("{} attach --no-statusline {}", coop_bin(), coop.base_url());
    let tmux = TmuxOracle::new(&attach_cmd, COLS, ROWS)?;
    let attach_lines = tmux.wait_for_text(sentinel, WAIT)?;

    let server_lines = coop.fetch_screen_lines()?;

    compare_output_region("server", &server_lines, "attach", &attach_lines, sentinel)?;

    Ok(())
}
