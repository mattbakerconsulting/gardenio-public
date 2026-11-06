// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! gRPC transport implementing the `Coop` service defined in `coop.v1`.

pub mod convert;
mod service;

use std::pin::Pin;
use std::sync::Arc;

use tokio::sync::{broadcast, mpsc};
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Status};

use crate::transport::state::Store;

/// Generated protobuf types for the `coop.v1` package.
pub mod proto {
    tonic::include_proto!("coop.v1");
}

/// Spawn a task that reads from a broadcast receiver, maps each event through
/// `map_fn`, and forwards the result into a gRPC response stream.
fn spawn_broadcast_stream<E, T, F>(rx: broadcast::Receiver<E>, map_fn: F) -> GrpcStream<T>
where
    E: Clone + Send + 'static,
    T: Send + 'static,
    F: Fn(E) -> Option<T> + Send + 'static,
{
    let (tx, receiver) = mpsc::channel(16);
    tokio::spawn(async move {
        let mut rx = rx;
        loop {
            match rx.recv().await {
                Ok(event) => {
                    if let Some(item) = map_fn(event) {
                        if tx.send(Ok(item)).await.is_err() {
                            break;
                        }
                    }
                }
                Err(broadcast::error::RecvError::Lagged(_)) => {}
                Err(broadcast::error::RecvError::Closed) => break,
            }
        }
    });
    Box::pin(ReceiverStream::new(receiver))
}

/// gRPC implementation of the `coop.v1.Coop` service.
pub struct CoopGrpc {
    state: Arc<Store>,
}

impl CoopGrpc {
    /// Create a new gRPC service backed by the given shared state.
    pub fn new(state: Arc<Store>) -> Self {
        Self { state }
    }

    /// Build a [`tonic`] router for this service.
    ///
    /// When an auth token is configured, an interceptor validates Bearer
    /// tokens on all RPCs except `GetHealth` and `GetReady`.
    pub fn into_router(self) -> tonic::transport::server::Router {
        let auth_token = self.state.config.auth_token.clone();
        let mut server = tonic::transport::Server::builder();
        if let Some(token) = auth_token {
            let interceptor = GrpcAuthInterceptor { token };
            server.add_service(proto::coop_server::CoopServer::with_interceptor(self, interceptor))
        } else {
            server.add_service(proto::coop_server::CoopServer::new(self))
        }
    }
}

/// gRPC interceptor that validates Bearer tokens on all RPCs.
///
/// Unlike the HTTP auth middleware which exempts health/ready probes,
/// gRPC auth applies uniformly — gRPC clients are orchestration tools
/// that always have the auth token available.
#[derive(Clone)]
struct GrpcAuthInterceptor {
    token: String,
}

impl tonic::service::Interceptor for GrpcAuthInterceptor {
    fn call(&mut self, req: Request<()>) -> Result<Request<()>, Status> {
        let header = req
            .metadata()
            .get("authorization")
            .and_then(|v| v.to_str().ok())
            .ok_or_else(|| Status::unauthenticated("missing authorization header"))?;

        let bearer = header
            .strip_prefix("Bearer ")
            .ok_or_else(|| Status::unauthenticated("invalid authorization scheme"))?;

        if crate::transport::auth::constant_time_eq(bearer, &self.token) {
            Ok(req)
        } else {
            Err(Status::unauthenticated("invalid token"))
        }
    }
}

type GrpcStream<T> = Pin<Box<dyn tokio_stream::Stream<Item = Result<T, Status>> + Send + 'static>>;

#[cfg(test)]
mod convert_tests;

#[cfg(test)]
mod service_tests;

#[cfg(test)]
mod transcript_tests;
