// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! `Coop` trait implementation — all gRPC RPC handlers.

use std::sync::atomic::Ordering;
use std::sync::Arc;
use tokio::sync::{broadcast, mpsc};
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Response, Status};

use super::convert::{
    profile_event_to_proto, prompt_to_proto, screen_text_snapshot_to_proto,
    screen_text_snapshot_to_response, transition_to_proto,
};
use super::{proto, spawn_broadcast_stream, CoopGrpc, GrpcStream};
use crate::error::ErrorCode;
use crate::event::OutputEvent;
use crate::start::StartConfig;
use crate::stop::StopConfig;
use crate::transport::handler::{
    compute_health, compute_status, error_message, extract_parked_fields, handle_input,
    handle_input_raw, handle_keys, handle_nudge, handle_resize, handle_respond, handle_signal,
    resolve_switch_profile, TransportQuestionAnswer,
};
use crate::transport::read_ring_combined;

#[tonic::async_trait]
impl proto::coop_server::Coop for CoopGrpc {
    // -- Terminal -------------------------------------------------------------

    async fn get_health(
        &self,
        _request: Request<proto::GetHealthRequest>,
    ) -> Result<Response<proto::GetHealthResponse>, Status> {
        let h = compute_health(&self.state).await;
        Ok(Response::new(proto::GetHealthResponse {
            status: h.status,
            pid: h.pid,
            uptime_secs: h.uptime_secs,
            agent: h.agent,
            ws_clients: h.ws_clients,
            terminal_cols: h.terminal_cols as i32,
            terminal_rows: h.terminal_rows as i32,
            ready: h.ready,
            session_id: h.session_id,
        }))
    }

    async fn get_ready(
        &self,
        _request: Request<proto::GetReadyRequest>,
    ) -> Result<Response<proto::GetReadyResponse>, Status> {
        let ready = self.state.ready.load(Ordering::Acquire);
        Ok(Response::new(proto::GetReadyResponse { ready }))
    }

    async fn get_screen(
        &self,
        request: Request<proto::GetScreenRequest>,
    ) -> Result<Response<proto::GetScreenResponse>, Status> {
        let req = request.into_inner();
        let screen = self.state.terminal.screen.read().await;
        let snap = screen.snapshot_text();
        Ok(Response::new(screen_text_snapshot_to_response(&snap, req.cursor)))
    }

    async fn get_status(
        &self,
        _request: Request<proto::GetStatusRequest>,
    ) -> Result<Response<proto::GetStatusResponse>, Status> {
        let st = compute_status(&self.state).await;
        Ok(Response::new(proto::GetStatusResponse {
            state: st.state,
            pid: st.pid,
            uptime_secs: st.uptime_secs,
            exit_code: st.exit_code,
            screen_seq: st.screen_seq,
            bytes_read: st.bytes_read,
            bytes_written: st.bytes_written,
            ws_clients: st.ws_clients,
            session_id: st.session_id,
        }))
    }

    type StreamOutputStream = GrpcStream<proto::OutputChunk>;

    async fn stream_output(
        &self,
        request: Request<proto::StreamOutputRequest>,
    ) -> Result<Response<Self::StreamOutputStream>, Status> {
        let from_offset = request.into_inner().from_offset;
        let (tx, rx) = mpsc::channel(64);

        // Replay buffered data from ring buffer
        {
            let ring = self.state.terminal.ring.read().await;
            // Clamp to oldest available offset so wrapped ring buffers
            // still return the most recent data instead of empty.
            let from_offset = from_offset.max(ring.oldest_offset());
            let data = read_ring_combined(&ring, from_offset);
            if !data.is_empty() {
                let _ = tx.send(Ok(proto::OutputChunk { data, offset: from_offset })).await;
            }
        }

        // Subscribe to live output
        let mut output_rx = self.state.channels.output_tx.subscribe();

        tokio::spawn(async move {
            loop {
                match output_rx.recv().await {
                    Ok(OutputEvent::Raw { data, offset }) => {
                        let chunk = proto::OutputChunk { data: data.to_vec(), offset };
                        if tx.send(Ok(chunk)).await.is_err() {
                            break;
                        }
                    }
                    Err(broadcast::error::RecvError::Lagged(_)) => {
                        // Skip missed messages
                    }
                    Err(broadcast::error::RecvError::Closed) => break,
                }
            }
        });

        Ok(Response::new(Box::pin(ReceiverStream::new(rx))))
    }

    type StreamScreenStream = GrpcStream<proto::ScreenSnapshot>;

    async fn stream_screen(
        &self,
        _request: Request<proto::StreamScreenRequest>,
    ) -> Result<Response<Self::StreamScreenStream>, Status> {
        let (tx, rx) = mpsc::channel(16);
        let mut screen_rx = self.state.channels.screen_tx.subscribe();
        let state = Arc::clone(&self.state);

        tokio::spawn(async move {
            loop {
                match screen_rx.recv().await {
                    Ok(snap) => {
                        let proto_snap = screen_text_snapshot_to_proto(&snap);
                        if tx.send(Ok(proto_snap)).await.is_err() {
                            break;
                        }
                    }
                    Err(broadcast::error::RecvError::Lagged(skipped)) => {
                        let snap = state.terminal.screen.read().await.snapshot_text();
                        screen_rx = state.channels.screen_tx.subscribe();
                        let proto_snap = screen_text_snapshot_to_proto(&snap);
                        if tx.send(Ok(proto_snap)).await.is_err() {
                            break;
                        }
                        tracing::debug!(
                            skipped,
                            sequence = snap.sequence,
                            "recovered gRPC screen stream from broadcast lag with latest snapshot"
                        );
                    }
                    Err(broadcast::error::RecvError::Closed) => break,
                }
            }
        });

        Ok(Response::new(Box::pin(ReceiverStream::new(rx))))
    }

    async fn send_input(
        &self,
        request: Request<proto::SendInputRequest>,
    ) -> Result<Response<proto::SendInputResponse>, Status> {
        let req = request.into_inner();
        let len = handle_input(&self.state, req.text, req.enter).await;
        Ok(Response::new(proto::SendInputResponse { bytes_written: len }))
    }

    async fn send_input_raw(
        &self,
        request: Request<proto::SendInputRawRequest>,
    ) -> Result<Response<proto::SendInputRawResponse>, Status> {
        let req = request.into_inner();
        let len = handle_input_raw(&self.state, req.data).await;
        Ok(Response::new(proto::SendInputRawResponse { bytes_written: len }))
    }

    async fn send_keys(
        &self,
        request: Request<proto::SendKeysRequest>,
    ) -> Result<Response<proto::SendKeysResponse>, Status> {
        let req = request.into_inner();
        let len = handle_keys(&self.state, &req.keys).await.map_err(|bad_key| {
            ErrorCode::BadRequest.to_grpc_status(format!("unknown key: {bad_key}"))
        })?;
        Ok(Response::new(proto::SendKeysResponse { bytes_written: len }))
    }

    async fn resize(
        &self,
        request: Request<proto::ResizeRequest>,
    ) -> Result<Response<proto::ResizeResponse>, Status> {
        let req = request.into_inner();
        let cols: u16 = req
            .cols
            .try_into()
            .map_err(|_| ErrorCode::BadRequest.to_grpc_status("cols must be a positive u16"))?;
        let rows: u16 = req
            .rows
            .try_into()
            .map_err(|_| ErrorCode::BadRequest.to_grpc_status("rows must be a positive u16"))?;
        handle_resize(&self.state, cols, rows)
            .await
            .map_err(|code| code.to_grpc_status("cols and rows must be positive"))?;
        Ok(Response::new(proto::ResizeResponse { cols: cols as i32, rows: rows as i32 }))
    }

    async fn send_signal(
        &self,
        request: Request<proto::SendSignalRequest>,
    ) -> Result<Response<proto::SendSignalResponse>, Status> {
        let req = request.into_inner();
        handle_signal(&self.state, &req.signal).await.map_err(|bad_signal| {
            ErrorCode::BadRequest.to_grpc_status(format!("unknown signal: {bad_signal}"))
        })?;
        Ok(Response::new(proto::SendSignalResponse { delivered: true }))
    }

    // -- Agent ----------------------------------------------------------------

    async fn get_agent(
        &self,
        _request: Request<proto::GetAgentRequest>,
    ) -> Result<Response<proto::GetAgentResponse>, Status> {
        let agent = self.state.driver.agent_state.read().await;
        let screen = self.state.terminal.screen.read().await;

        let detection = self.state.driver.detection.read().await;

        let (parked_reason, resume_at_epoch_ms) = extract_parked_fields(&agent);
        let session_id = self.state.session_id.read().await.clone();
        Ok(Response::new(proto::GetAgentResponse {
            agent: self.state.config.agent.to_string(),
            state: agent.as_str().to_owned(),
            since_seq: self.state.driver.state_seq.load(Ordering::Acquire),
            screen_seq: screen.seq(),
            detection_tier: detection.tier_str(),
            detection_cause: detection.cause.clone(),
            prompt: agent.prompt().map(prompt_to_proto),
            error_detail: self.state.driver.error.read().await.as_ref().map(|e| e.detail.clone()),
            error_category: self
                .state
                .driver
                .error
                .read()
                .await
                .as_ref()
                .map(|e| e.category.as_str().to_owned()),
            last_message: self.state.driver.last_message.read().await.clone(),
            session_id,
            parked_reason,
            resume_at_epoch_ms,
        }))
    }

    async fn nudge(
        &self,
        request: Request<proto::NudgeRequest>,
    ) -> Result<Response<proto::NudgeResponse>, Status> {
        let req = request.into_inner();
        match handle_nudge(&self.state, &req.message).await {
            Ok(outcome) => Ok(Response::new(proto::NudgeResponse {
                delivered: outcome.delivered,
                state_before: outcome.state_before,
                reason: outcome.reason,
            })),
            Err(code) => Err(code.to_grpc_status(error_message(code))),
        }
    }

    async fn respond(
        &self,
        request: Request<proto::RespondRequest>,
    ) -> Result<Response<proto::RespondResponse>, Status> {
        let req = request.into_inner();
        let answers: Vec<TransportQuestionAnswer> = req
            .answers
            .iter()
            .map(|a| TransportQuestionAnswer { option: a.option, text: a.text.clone() })
            .collect();
        match handle_respond(&self.state, req.accept, req.option, req.text.as_deref(), &answers)
            .await
        {
            Ok(outcome) => Ok(Response::new(proto::RespondResponse {
                delivered: outcome.delivered,
                prompt_type: outcome.prompt_type,
                reason: outcome.reason,
            })),
            Err(code) => Err(code.to_grpc_status(error_message(code))),
        }
    }

    type StreamAgentStream = GrpcStream<proto::TransitionEvent>;

    async fn stream_agent(
        &self,
        _request: Request<proto::StreamAgentRequest>,
    ) -> Result<Response<Self::StreamAgentStream>, Status> {
        let state_rx = self.state.channels.state_tx.subscribe();
        let stream = spawn_broadcast_stream(state_rx, |event| Some(transition_to_proto(&event)));
        Ok(Response::new(stream))
    }

    type StreamPromptOutcomesStream = GrpcStream<proto::PromptOutcomeEvent>;

    async fn stream_prompt_outcomes(
        &self,
        _request: Request<proto::StreamPromptOutcomesRequest>,
    ) -> Result<Response<Self::StreamPromptOutcomesStream>, Status> {
        let prompt_rx = self.state.channels.prompt_tx.subscribe();
        let stream = spawn_broadcast_stream(prompt_rx, |event| {
            Some(proto::PromptOutcomeEvent {
                source: event.source,
                r#type: event.r#type,
                subtype: event.subtype,
                option: event.option,
            })
        });
        Ok(Response::new(stream))
    }

    // -- Raw streams ----------------------------------------------------------

    type StreamRawHooksStream = GrpcStream<proto::RawHookEvent>;

    async fn stream_raw_hooks(
        &self,
        _request: Request<proto::StreamRawHooksRequest>,
    ) -> Result<Response<Self::StreamRawHooksStream>, Status> {
        let hook_rx = self.state.channels.hook_tx.subscribe();
        let stream = spawn_broadcast_stream(hook_rx, |event| {
            Some(proto::RawHookEvent { json: event.json.to_string() })
        });
        Ok(Response::new(stream))
    }

    type StreamRawMessagesStream = GrpcStream<proto::RawMessageEvent>;

    async fn stream_raw_messages(
        &self,
        _request: Request<proto::StreamRawMessagesRequest>,
    ) -> Result<Response<Self::StreamRawMessagesStream>, Status> {
        let message_rx = self.state.channels.message_tx.subscribe();
        let stream = spawn_broadcast_stream(message_rx, |event| {
            Some(proto::RawMessageEvent { json: event.json.to_string(), source: event.source })
        });
        Ok(Response::new(stream))
    }

    // -- Transcripts ----------------------------------------------------------

    async fn list_transcripts(
        &self,
        _request: Request<proto::ListTranscriptsRequest>,
    ) -> Result<Response<proto::ListTranscriptsResponse>, Status> {
        let list = self.state.transcript.list().await;
        let transcripts = list
            .into_iter()
            .map(|m| proto::TranscriptMeta {
                number: m.number,
                timestamp: m.timestamp,
                line_count: m.line_count,
                byte_size: m.byte_size,
            })
            .collect();
        Ok(Response::new(proto::ListTranscriptsResponse { transcripts }))
    }

    async fn get_transcript(
        &self,
        request: Request<proto::GetTranscriptRequest>,
    ) -> Result<Response<proto::GetTranscriptResponse>, Status> {
        let number = request.into_inner().number;
        let content = self
            .state
            .transcript
            .get_content(number)
            .await
            .map_err(|e| Status::not_found(format!("{e}")))?;
        Ok(Response::new(proto::GetTranscriptResponse { number, content }))
    }

    async fn catchup_transcripts(
        &self,
        request: Request<proto::CatchupTranscriptsRequest>,
    ) -> Result<Response<proto::CatchupTranscriptsResponse>, Status> {
        let req = request.into_inner();
        let resp = self
            .state
            .transcript
            .catchup(req.since_transcript, req.since_line)
            .await
            .map_err(|e| Status::internal(format!("{e}")))?;
        Ok(Response::new(proto::CatchupTranscriptsResponse {
            transcripts: resp
                .transcripts
                .into_iter()
                .map(|t| proto::CatchupTranscript {
                    number: t.number,
                    timestamp: t.timestamp,
                    lines: t.lines,
                })
                .collect(),
            live_lines: resp.live_lines,
            current_transcript: resp.current_transcript,
            current_line: resp.current_line,
        }))
    }

    type StreamTranscriptEventsStream = GrpcStream<proto::TranscriptEvent>;

    async fn stream_transcript_events(
        &self,
        _request: Request<proto::StreamTranscriptEventsRequest>,
    ) -> Result<Response<Self::StreamTranscriptEventsStream>, Status> {
        let transcript_rx = self.state.transcript.transcript_tx.subscribe();
        let stream = spawn_broadcast_stream(transcript_rx, |event| {
            Some(proto::TranscriptEvent {
                number: event.number,
                timestamp: event.timestamp,
                line_count: event.line_count,
                seq: event.seq,
            })
        });
        Ok(Response::new(stream))
    }

    // -- Stop hook ------------------------------------------------------------

    async fn get_stop_config(
        &self,
        _request: Request<proto::GetStopConfigRequest>,
    ) -> Result<Response<proto::GetStopConfigResponse>, Status> {
        let config = self.state.stop.config.read().await;
        let json = serde_json::to_string(&*config)
            .map_err(|e| Status::internal(format!("serialize error: {e}")))?;
        Ok(Response::new(proto::GetStopConfigResponse { config_json: json }))
    }

    async fn put_stop_config(
        &self,
        request: Request<proto::PutStopConfigRequest>,
    ) -> Result<Response<proto::PutStopConfigResponse>, Status> {
        let req = request.into_inner();
        let new_config: StopConfig = serde_json::from_str(&req.config_json)
            .map_err(|e| Status::invalid_argument(format!("invalid config JSON: {e}")))?;
        *self.state.stop.config.write().await = new_config;
        Ok(Response::new(proto::PutStopConfigResponse { updated: true }))
    }

    async fn resolve_stop(
        &self,
        request: Request<proto::ResolveStopRequest>,
    ) -> Result<Response<proto::ResolveStopResponse>, Status> {
        let req = request.into_inner();
        let body: serde_json::Value = serde_json::from_str(&req.body_json)
            .map_err(|e| Status::invalid_argument(format!("invalid JSON: {e}")))?;
        self.state.stop.resolve(body).await.map_err(Status::invalid_argument)?;
        Ok(Response::new(proto::ResolveStopResponse { accepted: true }))
    }

    type StreamStopEventsStream = GrpcStream<proto::StopEvent>;

    async fn stream_stop_events(
        &self,
        _request: Request<proto::StreamStopEventsRequest>,
    ) -> Result<Response<Self::StreamStopEventsStream>, Status> {
        let stop_rx = self.state.stop.stop_tx.subscribe();
        let stream = spawn_broadcast_stream(stop_rx, |event| {
            Some(proto::StopEvent {
                r#type: event.r#type.as_str().to_owned(),
                signal_json: event.signal.map(|v| v.to_string()),
                error_detail: event.error_detail,
                seq: event.seq,
            })
        });
        Ok(Response::new(stream))
    }

    // -- Start hook -----------------------------------------------------------

    async fn get_start_config(
        &self,
        _request: Request<proto::GetStartConfigRequest>,
    ) -> Result<Response<proto::GetStartConfigResponse>, Status> {
        let config = self.state.start.config.read().await;
        let json = serde_json::to_string(&*config)
            .map_err(|e| Status::internal(format!("serialize error: {e}")))?;
        Ok(Response::new(proto::GetStartConfigResponse { config_json: json }))
    }

    async fn put_start_config(
        &self,
        request: Request<proto::PutStartConfigRequest>,
    ) -> Result<Response<proto::PutStartConfigResponse>, Status> {
        let req = request.into_inner();
        let new_config: StartConfig = serde_json::from_str(&req.config_json)
            .map_err(|e| Status::invalid_argument(format!("invalid config JSON: {e}")))?;
        *self.state.start.config.write().await = new_config;
        Ok(Response::new(proto::PutStartConfigResponse { updated: true }))
    }

    type StreamStartEventsStream = GrpcStream<proto::StartEvent>;

    async fn stream_start_events(
        &self,
        _request: Request<proto::StreamStartEventsRequest>,
    ) -> Result<Response<Self::StreamStartEventsStream>, Status> {
        let start_rx = self.state.start.start_tx.subscribe();
        let stream = spawn_broadcast_stream(start_rx, |event| {
            Some(proto::StartEvent {
                source: event.source,
                session_id: event.session_id,
                injected: event.injected,
                seq: event.seq,
            })
        });
        Ok(Response::new(stream))
    }

    // -- Recording ------------------------------------------------------------

    async fn get_recording(
        &self,
        _request: Request<proto::GetRecordingRequest>,
    ) -> Result<Response<proto::GetRecordingResponse>, Status> {
        let status = self.state.record.status();
        Ok(Response::new(proto::GetRecordingResponse {
            enabled: status.enabled,
            path: status.path.unwrap_or_default(),
            entries: status.entries,
        }))
    }

    async fn put_recording(
        &self,
        request: Request<proto::PutRecordingRequest>,
    ) -> Result<Response<proto::PutRecordingResponse>, Status> {
        let req = request.into_inner();
        if req.enabled {
            self.state.record.enable().await;
        } else {
            self.state.record.disable();
        }
        let status = self.state.record.status();
        Ok(Response::new(proto::PutRecordingResponse {
            enabled: status.enabled,
            path: status.path.unwrap_or_default(),
        }))
    }

    async fn catchup_recording(
        &self,
        request: Request<proto::CatchupRecordingRequest>,
    ) -> Result<Response<proto::CatchupRecordingResponse>, Status> {
        let req = request.into_inner();
        let entries = self.state.record.catchup(req.since_seq).await;
        let proto_entries: Vec<proto::RecordingEntryProto> = entries
            .into_iter()
            .map(|e| proto::RecordingEntryProto {
                ts: e.ts,
                seq: e.seq,
                kind: e.kind,
                detail_json: e.detail.to_string(),
                screen_json: serde_json::to_string(&e.screen).unwrap_or_default(),
            })
            .collect();
        Ok(Response::new(proto::CatchupRecordingResponse { entries: proto_entries }))
    }

    type StreamRecordingEventsStream = GrpcStream<proto::RecordingEntryProto>;

    async fn stream_recording_events(
        &self,
        _request: Request<proto::StreamRecordingEventsRequest>,
    ) -> Result<Response<Self::StreamRecordingEventsStream>, Status> {
        let record_rx = self.state.record.record_tx.subscribe();
        let stream = spawn_broadcast_stream(record_rx, |event| {
            Some(proto::RecordingEntryProto {
                ts: event.ts,
                seq: event.seq,
                kind: event.kind,
                detail_json: event.detail.to_string(),
                screen_json: serde_json::to_string(&event.screen).unwrap_or_default(),
            })
        });
        Ok(Response::new(stream))
    }

    // -- Usage tracking -------------------------------------------------------

    async fn get_session_usage(
        &self,
        _request: Request<proto::GetSessionUsageRequest>,
    ) -> Result<Response<proto::GetSessionUsageResponse>, Status> {
        let snap = self.state.usage.snapshot().await;
        Ok(Response::new(proto::GetSessionUsageResponse {
            input_tokens: snap.input_tokens,
            output_tokens: snap.output_tokens,
            cache_read_tokens: snap.cache_read_tokens,
            cache_write_tokens: snap.cache_write_tokens,
            request_count: snap.request_count,
            total_api_ms: snap.total_api_ms,
            uptime_secs: self.state.config.started_at.elapsed().as_secs() as i64,
        }))
    }

    type StreamUsageEventsStream = GrpcStream<proto::UsageEvent>;

    async fn stream_usage_events(
        &self,
        _request: Request<proto::StreamUsageEventsRequest>,
    ) -> Result<Response<Self::StreamUsageEventsStream>, Status> {
        let usage_rx = self.state.usage.usage_tx.subscribe();
        let stream = spawn_broadcast_stream(usage_rx, |event| {
            let snap = &event.cumulative;
            Some(proto::UsageEvent {
                input_tokens: snap.input_tokens,
                output_tokens: snap.output_tokens,
                cache_read_tokens: snap.cache_read_tokens,
                cache_write_tokens: snap.cache_write_tokens,
                request_count: snap.request_count,
                total_api_ms: snap.total_api_ms,
                seq: event.seq,
            })
        });
        Ok(Response::new(stream))
    }

    // -- Profile management ---------------------------------------------------

    async fn register_profiles(
        &self,
        request: Request<proto::RegisterProfilesRequest>,
    ) -> Result<Response<proto::RegisterProfilesResponse>, Status> {
        let req = request.into_inner();
        let entries: Vec<crate::profile::ProfileEntry> = req
            .profiles
            .into_iter()
            .map(|p| crate::profile::ProfileEntry {
                name: p.name,
                credentials: p.credentials,
                usable: p.usable,
            })
            .collect();
        let count = entries.len();
        self.state.register_profiles(entries).await;
        Ok(Response::new(proto::RegisterProfilesResponse { registered: count as u32 }))
    }

    async fn list_profiles(
        &self,
        _request: Request<proto::ListProfilesRequest>,
    ) -> Result<Response<proto::ListProfilesResponse>, Status> {
        let profiles = self.state.profile.list().await;
        let mode = self.state.profile.mode().as_str().to_owned();
        let active_profile = self.state.profile.active_name().await;
        Ok(Response::new(proto::ListProfilesResponse {
            profiles: profiles
                .into_iter()
                .map(|p| proto::ProfileInfo {
                    name: p.name,
                    status: p.status,
                    cooldown_remaining_secs: p.cooldown_remaining_secs,
                })
                .collect(),
            mode,
            active_profile,
        }))
    }

    async fn get_profile_mode(
        &self,
        _request: Request<proto::GetProfileModeRequest>,
    ) -> Result<Response<proto::ProfileModeResponse>, Status> {
        let mode = self.state.profile.mode().as_str().to_owned();
        Ok(Response::new(proto::ProfileModeResponse { mode }))
    }

    async fn set_profile_mode(
        &self,
        request: Request<proto::SetProfileModeRequest>,
    ) -> Result<Response<proto::ProfileModeResponse>, Status> {
        let req = request.into_inner();
        let mode: crate::profile::ProfileMode = req
            .mode
            .parse()
            .map_err(|_| Status::invalid_argument("invalid mode: expected auto or manual"))?;
        self.state.profile.set_mode(mode);
        Ok(Response::new(proto::ProfileModeResponse { mode: mode.as_str().to_owned() }))
    }

    type StreamProfileEventsStream = GrpcStream<proto::ProfileEvent>;

    async fn stream_profile_events(
        &self,
        _request: Request<proto::StreamProfileEventsRequest>,
    ) -> Result<Response<Self::StreamProfileEventsStream>, Status> {
        let profile_rx = self.state.profile.profile_tx.subscribe();
        let stream =
            spawn_broadcast_stream(profile_rx, |event| Some(profile_event_to_proto(&event)));
        Ok(Response::new(stream))
    }

    // -- Session management ---------------------------------------------------

    async fn switch_session(
        &self,
        request: Request<proto::SwitchSessionRequest>,
    ) -> Result<Response<proto::SwitchSessionResponse>, Status> {
        let req = request.into_inner();
        let mut switch_req = crate::switch::SwitchRequest {
            credentials: if req.credentials.is_empty() { None } else { Some(req.credentials) },
            force: req.force,
            profile: req.profile,
        };
        resolve_switch_profile(&self.state, &mut switch_req)
            .await
            .map_err(|code| code.to_grpc_status("unknown profile"))?;
        match self.state.switch.switch_tx.try_send(switch_req) {
            Ok(()) => Ok(Response::new(proto::SwitchSessionResponse { scheduled: true })),
            Err(tokio::sync::mpsc::error::TrySendError::Full(_)) => {
                Err(ErrorCode::SwitchInProgress.to_grpc_status("a switch is already in progress"))
            }
            Err(tokio::sync::mpsc::error::TrySendError::Closed(_)) => {
                Err(ErrorCode::Internal.to_grpc_status("switch channel closed"))
            }
        }
    }

    // -- Lifecycle ------------------------------------------------------------

    async fn restart_session(
        &self,
        _request: Request<proto::RestartSessionRequest>,
    ) -> Result<Response<proto::RestartSessionResponse>, Status> {
        let req = crate::switch::SwitchRequest { credentials: None, force: true, profile: None };
        match self.state.switch.switch_tx.try_send(req) {
            Ok(()) => Ok(Response::new(proto::RestartSessionResponse { scheduled: true })),
            Err(tokio::sync::mpsc::error::TrySendError::Full(_)) => {
                Err(ErrorCode::SwitchInProgress.to_grpc_status("a switch is already in progress"))
            }
            Err(tokio::sync::mpsc::error::TrySendError::Closed(_)) => {
                Err(ErrorCode::Internal.to_grpc_status("switch channel closed"))
            }
        }
    }

    async fn shutdown(
        &self,
        _request: Request<proto::ShutdownRequest>,
    ) -> Result<Response<proto::ShutdownResponse>, Status> {
        self.state.lifecycle.shutdown.cancel();
        Ok(Response::new(proto::ShutdownResponse { accepted: true }))
    }
}
