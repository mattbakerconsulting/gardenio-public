// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use clap::{Parser, Subcommand};
use tracing::error;

use coopmux::config::MuxConfig;

#[derive(Parser)]
#[command(name = "coopmux", version, about = "PTY multiplexing proxy for coop instances.")]
struct Cli {
    #[command(flatten)]
    config: MuxConfig,

    /// NATS server URL for credential event publishing (e.g. "nats://nats:4222").
    /// When set, credential events are published to NATS.
    #[arg(long, env = "COOP_MUX_NATS_URL")]
    nats_url: Option<String>,

    /// Auth token for the NATS connection.
    #[arg(long, env = "COOP_MUX_NATS_TOKEN")]
    nats_token: Option<String>,

    /// NATS username. When set, the token is sent as this user's PASSWORD —
    /// multi-user server auth (nats.auth.subjectIsolation) only supports
    /// user/password entries.
    #[arg(long, env = "COOP_MUX_NATS_USER")]
    nats_user: Option<String>,

    /// Subject prefix for NATS credential event publishing (default: "coop").
    #[arg(long, default_value = "coop", env = "COOP_MUX_NATS_PREFIX")]
    nats_prefix: String,

    /// NATS server URL for relay session auto-discovery (e.g. "nats://bd-daemon:4222").
    /// When set, coopmux subscribes to `{relay-prefix}.session.>` for local agents.
    #[arg(long, env = "COOP_MUX_NATS_RELAY_URL")]
    nats_relay_url: Option<String>,

    /// Auth token for the NATS relay connection.
    #[arg(long, env = "COOP_MUX_NATS_RELAY_TOKEN")]
    nats_relay_token: Option<String>,

    /// NATS username for the relay connection. When set, the relay token is
    /// sent as this user's PASSWORD — multi-user server auth
    /// (nats.auth.subjectIsolation) only supports user/password entries.
    #[arg(long, env = "COOP_MUX_NATS_RELAY_USER")]
    nats_relay_user: Option<String>,

    /// Subject prefix for NATS relay session discovery (default: "coop.mux").
    #[arg(long, default_value = "coop.mux", env = "COOP_MUX_NATS_RELAY_PREFIX")]
    nats_relay_prefix: String,

    #[command(subcommand)]
    command: Option<Commands>,
}

#[derive(Subcommand)]
enum Commands {
    /// Open the mux dashboard in a browser.
    Open(OpenArgs),
}

#[derive(clap::Args)]
struct OpenArgs {
    /// Mux server URL. Defaults to COOP_MUX_URL or http://{host}:{port}.
    #[arg(env = "COOP_MUX_URL")]
    url: Option<String>,
}

#[tokio::main]
async fn main() {
    // Install ring as the default rustls CryptoProvider before any TLS use.
    // This replaces aws-lc-rs, enabling cross-compilation to macOS.
    let _ = rustls::crypto::ring::default_provider().install_default();

    let cli = Cli::parse();

    match cli.command {
        Some(Commands::Open(args)) => {
            std::process::exit(open_dashboard(&cli.config, &args));
        }
        None => {
            tracing_subscriber::fmt()
                .with_env_filter(
                    tracing_subscriber::EnvFilter::try_from_default_env()
                        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
                )
                .init();

            let nats = cli.nats_url.map(|url| coopmux::NatsConfig {
                url,
                token: cli.nats_token,
                user: cli.nats_user,
                prefix: cli.nats_prefix,
            });
            let nats_relay = cli.nats_relay_url.map(|url| coopmux::NatsRelayConfig {
                url,
                token: cli.nats_relay_token,
                user: cli.nats_relay_user,
                prefix: cli.nats_relay_prefix,
            });
            if let Err(e) = coopmux::run(cli.config, nats, nats_relay).await {
                error!("fatal: {e:#}");
                std::process::exit(1);
            }
        }
    }
}

fn open_dashboard(config: &MuxConfig, args: &OpenArgs) -> i32 {
    let base = if let Some(ref url) = args.url {
        url.trim_end_matches('/').to_owned()
    } else {
        format!("http://{}:{}", config.host, config.port)
    };

    let mut url = format!("{base}/mux");
    if let Some(ref token) = config.auth_token {
        url.push_str(&format!("?token={token}"));
    }

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
