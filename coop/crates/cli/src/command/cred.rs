// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! `coop cred` — manage mux credentials from the CLI.
//!
//! Talks to the mux server's credential endpoints via `COOP_MUX_URL`.

/// CLI arguments for `coop cred`.
#[derive(Debug, clap::Args)]
pub struct CredArgs {
    /// Bearer token for the mux endpoint. Overrides `GARDENIO_TOKEN`,
    /// `COOP_MUX_TOKEN`, and `COOP_BROKER_TOKEN` env vars when set. Use with
    /// the edge-auth path (`cli.gardenio.<env>/coop`) — value is typically a
    /// user-scoped credential such as a GitLab PAT. Distinct from the
    /// per-credential `--token` accepted by `cred new` / `cred set`, which
    /// supplies the credential value being stored.
    #[arg(long = "mux-token", global = true)]
    pub mux_token: Option<String>,

    #[command(subcommand)]
    pub command: CredCommand,
}

#[derive(Debug, clap::Subcommand)]
pub enum CredCommand {
    /// List all credential accounts and their status.
    List,
    /// Create a new credential account.
    New(NewArgs),
    /// Set tokens for an existing account.
    Set(SetArgs),
    /// Trigger OAuth re-authentication for an account.
    Reauth(ReauthArgs),
}

#[derive(Debug, clap::Args)]
pub struct NewArgs {
    /// Account name.
    pub name: String,
    /// Provider identifier (e.g. "claude", "openai").
    #[arg(long)]
    pub provider: String,
    /// Env var name for the credential (falls back to provider default).
    #[arg(long)]
    pub env_key: Option<String>,
    /// Access token to set immediately.
    #[arg(long)]
    pub token: Option<String>,
    /// Refresh token (optional).
    #[arg(long)]
    pub refresh_token: Option<String>,
    /// Token TTL in seconds (optional).
    #[arg(long)]
    pub expires_in: Option<u64>,
    /// Disable OAuth reauth/refresh for this account.
    #[arg(long)]
    pub no_reauth: bool,
}

#[derive(Debug, clap::Args)]
pub struct SetArgs {
    /// Account name.
    pub account: String,
    /// Access token.
    #[arg(long)]
    pub token: String,
    /// Refresh token (optional).
    #[arg(long)]
    pub refresh_token: Option<String>,
    /// Token TTL in seconds (optional).
    #[arg(long)]
    pub expires_in: Option<u64>,
}

#[derive(Debug, clap::Args)]
pub struct ReauthArgs {
    /// Account name (defaults to first account).
    pub account: Option<String>,
}

/// Run the `coop cred` subcommand. Returns a process exit code.
pub async fn run(args: &CredArgs) -> i32 {
    let mux_url = match std::env::var("COOP_MUX_URL") {
        Ok(u) => u.trim_end_matches('/').to_owned(),
        Err(_) => {
            eprintln!("error: COOP_MUX_URL is not set");
            return 2;
        }
    };

    let mux_token = super::resolve_mux_token(args.mux_token.as_deref());
    let client = reqwest::Client::builder()
        .user_agent(concat!("coop/", env!("CARGO_PKG_VERSION")))
        .timeout(std::time::Duration::from_secs(10))
        .build()
        .unwrap_or_default();

    match &args.command {
        CredCommand::List => cmd_list(&client, &mux_url, mux_token.as_deref()).await,
        CredCommand::New(new_args) => {
            cmd_new(&client, &mux_url, mux_token.as_deref(), new_args).await
        }
        CredCommand::Set(set_args) => {
            cmd_set(&client, &mux_url, mux_token.as_deref(), set_args).await
        }
        CredCommand::Reauth(reauth) => {
            cmd_reauth(&client, &mux_url, mux_token.as_deref(), reauth).await
        }
    }
}

fn apply_auth(req: reqwest::RequestBuilder, token: Option<&str>) -> reqwest::RequestBuilder {
    match token {
        Some(t) => req.bearer_auth(t),
        None => req,
    }
}

async fn cmd_list(client: &reqwest::Client, mux_url: &str, token: Option<&str>) -> i32 {
    let url = format!("{mux_url}/api/v1/credentials/status");
    let resp = match apply_auth(client.get(&url), token).send().await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("error: {e}");
            return 1;
        }
    };

    let status = resp.status();
    let text = resp.text().await.unwrap_or_default();

    if status.is_success() {
        // Pretty-print the JSON table.
        match serde_json::from_str::<Vec<serde_json::Value>>(&text) {
            Ok(accounts) => {
                if accounts.is_empty() {
                    println!("No credential accounts configured.");
                } else {
                    println!("{:<20} {:<12} {:<10}", "ACCOUNT", "STATUS", "PROVIDER");
                    println!("{}", "-".repeat(42));
                    for acct in &accounts {
                        let name = acct.get("name").and_then(|v| v.as_str()).unwrap_or("?");
                        let status =
                            acct.get("status").and_then(|v| v.as_str()).unwrap_or("unknown");
                        let provider = acct.get("provider").and_then(|v| v.as_str()).unwrap_or("?");
                        println!("{name:<20} {status:<12} {provider:<10}");
                    }
                }
                0
            }
            Err(_) => {
                // Fallback: print raw JSON.
                println!("{text}");
                0
            }
        }
    } else {
        eprintln!("error ({status}): {text}");
        1
    }
}

async fn cmd_new(
    client: &reqwest::Client,
    mux_url: &str,
    token: Option<&str>,
    args: &NewArgs,
) -> i32 {
    let url = format!("{mux_url}/api/v1/credentials/new");
    let body = serde_json::json!({
        "name": args.name,
        "provider": args.provider,
        "env_key": args.env_key,
        "token": args.token,
        "refresh_token": args.refresh_token,
        "expires_in": args.expires_in,
        "reauth": !args.no_reauth,
    });

    let resp = match apply_auth(client.post(&url), token).json(&body).send().await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("error: {e}");
            return 1;
        }
    };

    let status = resp.status();
    let text = resp.text().await.unwrap_or_default();

    if status.is_success() {
        println!("Created account '{}'.", args.name);
        0
    } else {
        eprintln!("error ({status}): {text}");
        1
    }
}

async fn cmd_set(
    client: &reqwest::Client,
    mux_url: &str,
    token: Option<&str>,
    args: &SetArgs,
) -> i32 {
    let url = format!("{mux_url}/api/v1/credentials/set");
    let body = serde_json::json!({
        "account": args.account,
        "token": args.token,
        "refresh_token": args.refresh_token,
        "expires_in": args.expires_in,
    });

    let resp = match apply_auth(client.post(&url), token).json(&body).send().await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("error: {e}");
            return 1;
        }
    };

    let status = resp.status();
    let text = resp.text().await.unwrap_or_default();

    if status.is_success() {
        println!("Set token for account '{}'.", args.account);
        0
    } else {
        eprintln!("error ({status}): {text}");
        1
    }
}

async fn cmd_reauth(
    client: &reqwest::Client,
    mux_url: &str,
    token: Option<&str>,
    args: &ReauthArgs,
) -> i32 {
    let url = format!("{mux_url}/api/v1/credentials/reauth");
    let body = serde_json::json!({
        "account": args.account,
    });

    let resp = match apply_auth(client.post(&url), token).json(&body).send().await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("error: {e}");
            return 1;
        }
    };

    let status = resp.status();
    let text = resp.text().await.unwrap_or_default();

    if status.is_success() {
        if let Ok(val) = serde_json::from_str::<serde_json::Value>(&text) {
            let auth_url = val.get("auth_url").and_then(|v| v.as_str());
            let user_code = val.get("user_code").and_then(|v| v.as_str());

            if let Some(code) = user_code {
                // Device code flow — show the code and verification URL.
                if let Some(url) = auth_url {
                    println!("Enter this code: {code}");
                    println!("  {url}");
                    #[cfg(target_os = "macos")]
                    {
                        let _ = std::process::Command::new("open").arg(url).spawn();
                    }
                    #[cfg(target_os = "linux")]
                    {
                        let _ = std::process::Command::new("xdg-open").arg(url).spawn();
                    }
                } else {
                    println!("Enter this code: {code}");
                }
                return 0;
            } else if let Some(url) = auth_url {
                // PKCE flow — open authorization URL.
                println!("Open this URL to authenticate:");
                println!("  {url}");
                #[cfg(target_os = "macos")]
                {
                    let _ = std::process::Command::new("open").arg(url).spawn();
                }
                #[cfg(target_os = "linux")]
                {
                    let _ = std::process::Command::new("xdg-open").arg(url).spawn();
                }
                return 0;
            }
        }
        println!("{text}");
        0
    } else {
        eprintln!("error ({status}): {text}");
        1
    }
}
