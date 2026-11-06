// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_prost_build::configure()
        .build_server(true)
        .build_client(true)
        .compile_protos(&["../../proto/coop/v1/coop.proto"], &["../../proto"])?;

    // Derive version from git tags so local builds show the real version
    // instead of the placeholder in Cargo.toml.
    if let Ok(output) =
        std::process::Command::new("git").args(["describe", "--tags", "--always"]).output()
    {
        let desc = String::from_utf8_lossy(&output.stdout).trim().to_string();
        if !desc.is_empty() {
            // Strip leading 'v' to match cargo semver convention.
            let version = desc.strip_prefix('v').unwrap_or(&desc);
            println!("cargo:rustc-env=CARGO_PKG_VERSION={version}");
        }
    }
    // Re-run if HEAD changes (new commit or tag).
    println!("cargo:rerun-if-changed=../../.git/HEAD");
    println!("cargo:rerun-if-changed=../../.git/refs/tags");

    Ok(())
}
