// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Layer B: Process-level rendering tests with tmux oracle.
//!
//! Runs claudeless in both a coop process and a tmux session. Compares coop's
//! HTTP screen API output against `tmux capture-pane -p` output.
//!
//! Requires `claudeless` and `tmux` in PATH.

mod rendering_support;

use std::time::Duration;

use rendering_support::{
    compare_output_region, require_claudeless, require_tmux, scenario_path, CoopScenario,
    TmuxOracle,
};

const COLS: u16 = 80;
const ROWS: u16 = 24;
const WAIT: Duration = Duration::from_secs(30);

// -- Test 1: Plain text -------------------------------------------------------

#[test]
fn coop_vs_tmux_plain_text() -> anyhow::Result<()> {
    require_claudeless();
    require_tmux();

    let sentinel = "The quick brown fox";
    let scenario = "render_plain.toml";

    // Start coop with claudeless.
    let coop = CoopScenario::start(scenario, "render plain", COLS, ROWS)?;
    let coop_lines = coop.wait_for_screen_text(sentinel, WAIT)?;

    // Start same command in tmux.
    let tmux_cmd = format!("claudeless --scenario {} 'render plain'", scenario_path(scenario));
    let tmux = TmuxOracle::new(&tmux_cmd, COLS, ROWS)?;
    let tmux_lines = tmux.wait_for_text(sentinel, WAIT)?;

    compare_output_region("coop", &coop_lines, "tmux", &tmux_lines, sentinel)?;

    Ok(())
}

// -- Test 2: ANSI colors ------------------------------------------------------

#[test]
fn coop_vs_tmux_colors() -> anyhow::Result<()> {
    require_claudeless();
    require_tmux();

    let sentinel = "Red text";
    let scenario = "render_colors.toml";

    let coop = CoopScenario::start(scenario, "render colors", COLS, ROWS)?;
    let coop_lines = coop.wait_for_screen_text(sentinel, WAIT)?;

    let tmux_cmd = format!("claudeless --scenario {} 'render colors'", scenario_path(scenario));
    let tmux = TmuxOracle::new(&tmux_cmd, COLS, ROWS)?;
    let tmux_lines = tmux.wait_for_text(sentinel, WAIT)?;

    // Compare text content (not ANSI — tmux capture-pane -p strips colors).
    compare_output_region("coop", &coop_lines, "tmux", &tmux_lines, sentinel)?;

    Ok(())
}

// -- Test 3: Scrollback -------------------------------------------------------

#[test]
fn coop_vs_tmux_scrollback() -> anyhow::Result<()> {
    require_claudeless();
    require_tmux();

    let sentinel = "Line 35";
    let scenario = "render_long_output.toml";

    let coop = CoopScenario::start(scenario, "render long", COLS, ROWS)?;
    let coop_lines = coop.wait_for_screen_text(sentinel, WAIT)?;

    let tmux_cmd = format!("claudeless --scenario {} 'render long'", scenario_path(scenario));
    let tmux = TmuxOracle::new(&tmux_cmd, COLS, ROWS)?;
    let tmux_lines = tmux.wait_for_text(sentinel, WAIT)?;

    // Both should have "Line 35" in the visible viewport.
    compare_output_region("coop", &coop_lines, "tmux", &tmux_lines, sentinel)?;

    Ok(())
}

// -- Test 4: UTF-8 ------------------------------------------------------------

#[test]
fn coop_vs_tmux_utf8() -> anyhow::Result<()> {
    require_claudeless();
    require_tmux();

    let sentinel = "Unicode:";
    let scenario = "render_utf8.toml";

    let coop = CoopScenario::start(scenario, "render utf8", COLS, ROWS)?;
    let coop_lines = coop.wait_for_screen_text(sentinel, WAIT)?;

    let tmux_cmd = format!("claudeless --scenario {} 'render utf8'", scenario_path(scenario));
    let tmux = TmuxOracle::new(&tmux_cmd, COLS, ROWS)?;
    let tmux_lines = tmux.wait_for_text(sentinel, WAIT)?;

    compare_output_region("coop", &coop_lines, "tmux", &tmux_lines, sentinel)?;

    Ok(())
}

// NOTE: Tool use rendering is not tested in Layer B because the coop server's
// hook-based tool detection flow differs fundamentally from claudeless running
// standalone. Tool rendering correctness is covered by Layer A (API-level) tests.
