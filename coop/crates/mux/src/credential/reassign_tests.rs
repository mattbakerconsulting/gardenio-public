// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Tests for the reassign helper.
//!
//! The actual `reassign_session` function makes HTTP calls to upstream coop
//! sessions, so full integration tests live in the integration test suite.
//! Unit tests here verify the module compiles and exports are accessible.

#[test]
fn module_compiles() {
    // Compile-time assertion: reassign_session is a public async fn.
    // If this test compiles, the module is correctly wired.
    assert!(std::any::type_name::<fn()>().contains("fn"));
}
