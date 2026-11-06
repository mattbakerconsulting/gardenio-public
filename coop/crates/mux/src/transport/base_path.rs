// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! BASE_PATH retrofit for serving behind a unified-host IngressRoute.
//!
//! Two pieces work together:
//!
//! 1. [`normalize`] turns the user-supplied prefix into a canonical form
//!    (`""` or `"/mux"`-shaped). All other helpers expect normalized input.
//! 2. [`inject_html`] injects a `<script>window.__COOP_BASE_PATH__="…"</script>`
//!    tag into the served dashboard HTML so the SPA can prefix its outgoing
//!    fetch / WebSocket URLs.
//!
//! Strip-prefix proxies (Traefik `stripPrefix: /mux`) hit the server at
//! root, so HTTP routes stay mounted at root unchanged. The dashboard is
//! additionally mounted at `/` when `base_path` is set so that a stripped
//! request for `/mux/` lands on the dashboard rather than a 404.

/// Normalize a user-supplied base path. Empty / `"/"` / whitespace return
/// `""`. Otherwise the result has a single leading slash and no trailing
/// slash (e.g. `"/mux"`, `"/api/v1/mux"`).
pub fn normalize(s: &str) -> String {
    let trimmed = s.trim().trim_end_matches('/');
    if trimmed.is_empty() {
        return String::new();
    }
    if trimmed.starts_with('/') {
        trimmed.to_owned()
    } else {
        format!("/{trimmed}")
    }
}

/// Inject a `<script>window.__COOP_BASE_PATH__="…"</script>` tag into the
/// dashboard HTML so the SPA prefixes its outgoing URLs. Returns the HTML
/// unchanged when `base_path` is empty.
///
/// The script is inserted immediately before `</head>` (or prepended if
/// no `</head>` is found, which would only happen for malformed HTML).
pub fn inject_html(html: &str, base_path: &str) -> String {
    if base_path.is_empty() {
        return html.to_owned();
    }
    let injection =
        format!("<script>window.__COOP_BASE_PATH__={}</script>", json_string_literal(base_path));
    match html.find("</head>") {
        Some(idx) => {
            let (head, tail) = html.split_at(idx);
            format!("{head}{injection}{tail}")
        }
        None => format!("{injection}{html}"),
    }
}

/// Render `s` as a JSON-safe double-quoted string literal. Used for the
/// HTML injection so the value embeds safely in a `<script>` tag and as
/// a JavaScript expression.
fn json_string_literal(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '<' => out.push_str("\\u003c"),
            '>' => out.push_str("\\u003e"),
            '&' => out.push_str("\\u0026"),
            c if (c as u32) < 0x20 => {
                out.push_str(&format!("\\u{:04x}", c as u32));
            }
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

#[cfg(test)]
#[path = "base_path_tests.rs"]
mod tests;
