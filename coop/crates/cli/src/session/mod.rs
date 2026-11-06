// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Session loop: core runtime orchestrating PTY, screen, ring buffer,
//! detection, and transport layers.

use std::sync::Arc;

use tokio_util::sync::CancellationToken;

use crate::backend::{Backend, Boxed};
use crate::driver::{Detector, ExitStatus, OptionParser, ScreenClassifier};
use crate::switch::SwitchRequest;
use crate::transport::Store;

mod groom;
pub mod run;
pub mod transition;

pub use run::Session;

/// Runtime objects for building a new [`Session`] (not derivable from [`Config`](crate::config::Config)).
pub struct SessionConfig {
    pub backend: Box<dyn Backend>,
    pub detectors: Vec<Box<dyn Detector>>,
    pub store: Arc<Store>,
    pub shutdown: CancellationToken,
    /// Driver-provided parser for extracting numbered option labels from
    /// rendered screen lines during prompt enrichment.
    pub option_parser: Option<OptionParser>,
    /// Driver-provided screen classifier, used to re-classify the *current*
    /// screen on demand (subtype-less prompt re-detection, and auto-dismiss
    /// re-validation after a `state_seq` bump — ka-0000002659).
    pub screen_classifier: Option<ScreenClassifier>,
}

impl SessionConfig {
    pub fn new(store: Arc<Store>, backend: impl Boxed) -> Self {
        Self {
            backend: backend.boxed(),
            store,
            detectors: Vec::new(),
            shutdown: CancellationToken::new(),
            option_parser: None,
            screen_classifier: None,
        }
    }

    pub fn with_detectors(mut self, detectors: Vec<Box<dyn Detector>>) -> Self {
        self.detectors = detectors;
        self
    }

    pub fn with_shutdown(mut self, shutdown: CancellationToken) -> Self {
        self.shutdown = shutdown;
        self
    }

    pub fn with_option_parser(mut self, parser: OptionParser) -> Self {
        self.option_parser = Some(parser);
        self
    }

    pub fn with_screen_classifier(mut self, classifier: ScreenClassifier) -> Self {
        self.screen_classifier = Some(classifier);
        self
    }
}

/// What happened when the session loop exited.
#[derive(Debug)]
pub enum SessionOutcome {
    /// The backend exited (process terminated).
    Exit(ExitStatus),
    /// A switch was requested and the backend has been drained.
    /// The caller should respawn with new credentials.
    Switch(SwitchRequest),
}

#[cfg(test)]
#[path = "../session_tests.rs"]
mod tests;
