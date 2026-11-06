// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::{prepare, HOOKS_JSON_ENV};

fn env<'a>(setup: &'a crate::driver::SessionSetup, key: &str) -> Option<&'a str> {
    setup.env_vars.iter().find(|(k, _)| k == key).map(|(_, v)| v.as_str())
}

#[test]
fn fresh_session_wires_pipe_token_and_hooks_file() -> anyhow::Result<()> {
    let setup = prepare("http://127.0.0.1:1234", false)?;
    let pipe = setup.hook_pipe_path.as_ref().ok_or_else(|| anyhow::anyhow!("no pipe"))?;
    assert_eq!(pipe.file_name().and_then(|n| n.to_str()), Some("hook.pipe"));
    assert!(pipe.starts_with(&setup.session_dir));
    assert_eq!(env(&setup, "COOP_HOOK_PIPE"), Some(pipe.display().to_string().as_str()));
    assert_eq!(env(&setup, "COOP_URL"), Some("http://127.0.0.1:1234"));
    assert!(setup.hook_token.is_some());
    assert!(env(&setup, "COOP_HOOK_TOKEN_FILE").is_some());
    let hooks = env(&setup, HOOKS_JSON_ENV).ok_or_else(|| anyhow::anyhow!("no hooks env"))?;
    assert!(std::path::Path::new(hooks).exists());
    Ok(())
}

#[test]
fn fresh_session_injects_no_command_line_args() -> anyhow::Result<()> {
    // Codex reads its config from $CODEX_HOME, which pod init owns.
    assert!(prepare("http://127.0.0.1:0", false)?.extra_args.is_empty());
    Ok(())
}

#[test]
fn pristine_session_has_no_hooks() -> anyhow::Result<()> {
    let setup = prepare("http://127.0.0.1:0", true)?;
    assert!(setup.hook_pipe_path.is_none());
    assert!(setup.hook_token.is_none());
    assert_eq!(env(&setup, "COOP_URL"), Some("http://127.0.0.1:0"));
    assert!(env(&setup, "COOP_HOOK_PIPE").is_none());
    Ok(())
}
