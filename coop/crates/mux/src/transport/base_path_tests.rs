// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

#[test]
fn normalize_handles_edge_cases() {
    for (input, want) in [
        ("", ""),
        ("   ", ""),
        ("/", ""),
        ("///", ""),
        ("mux", "/mux"),
        ("/mux", "/mux"),
        ("/mux/", "/mux"),
        ("/mux///", "/mux"),
        ("  /mux  ", "/mux"),
        ("/api/v1/mux", "/api/v1/mux"),
        ("api/v1/mux", "/api/v1/mux"),
    ] {
        assert_eq!(normalize(input), want, "normalize({input:?})");
    }
}

#[test]
fn inject_html_no_op_when_base_path_empty() {
    let html = "<html><head></head><body></body></html>";
    assert_eq!(inject_html(html, ""), html);
}

#[test]
fn inject_html_inserts_before_head_close() {
    let html = "<html><head><title>x</title></head><body></body></html>";
    let got = inject_html(html, "/mux");
    assert!(
        got.contains(r#"<script>window.__COOP_BASE_PATH__="/mux"</script></head>"#),
        "got: {got}"
    );
    // Existing head content preserved.
    assert!(got.contains("<title>x</title>"));
}

#[test]
fn inject_html_escapes_hostile_input() {
    let got = inject_html("<head></head>", r#"/mux"</script><script>alert(1)"#);
    // The dangerous chars should be JSON-escaped, not raw.
    assert!(!got.contains("</script><script>alert(1)"));
    assert!(got.contains("\\u003c"));
}

#[test]
fn inject_html_falls_back_when_no_head_close() {
    let html = "<html><body>oops</body></html>";
    let got = inject_html(html, "/mux");
    assert!(got.starts_with("<script>window.__COOP_BASE_PATH__"));
    assert!(got.ends_with(html));
}
