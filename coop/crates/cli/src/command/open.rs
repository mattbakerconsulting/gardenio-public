// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! `coop open` subcommand — opens the web terminal UI in a browser.

use clap::Args;

#[derive(Args)]
pub struct OpenArgs {
    /// Coop server URL (e.g. http://localhost:8080).
    /// Defaults to COOP_URL env var.
    #[arg(env = "COOP_URL")]
    pub url: Option<String>,

    /// Unix socket path (constructs a localhost URL instead).
    #[arg(long, env = "COOP_SOCKET")]
    pub socket: Option<String>,

    /// Port to use when neither URL nor socket is specified.
    #[arg(long, default_value = "8080")]
    pub port: u16,
}

impl OpenArgs {
    fn resolve_url(&self) -> String {
        if let Some(url) = &self.url {
            let url = url.trim_end_matches('/');
            // Normalise: strip /ws suffix if someone passes a WS URL
            if let Some(base) = url.strip_suffix("/ws") {
                base.to_string()
            } else {
                url.to_string()
            }
        } else {
            format!("http://localhost:{}", self.port)
        }
    }
}

pub fn run(args: &OpenArgs) -> i32 {
    let url = args.resolve_url();

    // On macOS use `open`, on Linux use `xdg-open`, on Windows use `start`.
    let cmd = if cfg!(target_os = "macos") {
        "open"
    } else if cfg!(target_os = "windows") {
        "start"
    } else {
        "xdg-open"
    };

    match std::process::Command::new(cmd).arg(&url).spawn() {
        Ok(_) => {
            eprintln!("Opening {url}");
            0
        }
        Err(e) => {
            eprintln!("Failed to open browser: {e}");
            eprintln!("Open manually: {url}");
            1
        }
    }
}
