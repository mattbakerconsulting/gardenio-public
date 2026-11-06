#!/usr/bin/env bun
// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
//
// gardenio-channel: MCP channel server that bridges the gardenio orchestrator
// to a running Claude Code session. Replaces unreliable PTY-based nudges
// with first-class Claude Code channel events.
//
// The bridge/controller POSTs messages to this server's HTTP endpoint.
// Messages are forwarded to Claude via the MCP channel protocol, arriving
// as <channel source="gardenio" ...> tags in Claude's context.
//
// Permission relay: when Claude hits a tool-approval prompt, Claude Code
// forwards it here. We POST it to the bridge, which shows it in Slack.
// The operator's verdict (allow/deny) comes back via POST /permission, from
// the agent's own mail subscriber on loopback: the bridge sends it as
// control:permission mail (gb cmd/gb/mail_permission.go).

import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
  ListToolsRequestSchema,
  CallToolRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";
import { createHash, timingSafeEqual } from "node:crypto";
import { z } from "zod";

const PORT = parseInt(process.env.GARDENIO_CHANNEL_PORT || "8789", 10);
const TOKEN = process.env.GARDENIO_CHANNEL_TOKEN || process.env.COOP_BROKER_TOKEN || "";
const CALLBACK_URL = process.env.GARDENIO_CHANNEL_CALLBACK_URL || "";
const AGENT_NAME = process.env.BOAT_AGENT || process.env.HOSTNAME || "";

// The bridge takes the asking agent of a permission request from the agent's
// own per-agent daemon token in X-Gardenio-Agent-Token, never from the body's
// agent alone (gd-eemeNhUt5I). The pod holds it as BEADS_AUTH_TOKEN; it can
// expire under a long-lived pod, so a 401 refreshes it once with
// BEADS_REFRESH_TOKEN against the daemon, as gb and kd do.
const AGENT_TOKEN_HEADER = "X-Gardenio-Agent-Token";
const BEADS_HTTP_ADDR = (process.env.BEADS_HTTP_ADDR || "").replace(/\/+$/, "");
let agentToken = process.env.BEADS_AUTH_TOKEN || process.env.BEADS_DAEMON_TOKEN || "";
let refreshToken = process.env.BEADS_REFRESH_TOKEN || "";

async function refreshAgentToken(): Promise<boolean> {
  if (!refreshToken || !BEADS_HTTP_ADDR) return false;
  try {
    const resp = await fetch(`${BEADS_HTTP_ADDR}/v1/auth/refresh`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: refreshToken }),
    });
    if (!resp.ok) return false;
    const body = (await resp.json()) as {
      access_token?: string;
      refresh_token?: string;
      refresh_token_rotated?: boolean;
    };
    if (!body.access_token) return false;
    agentToken = body.access_token;
    if (body.refresh_token_rotated && body.refresh_token) refreshToken = body.refresh_token;
    return true;
  } catch {
    return false;
  }
}

// Fail closed: an empty token would silently disable auth on 0.0.0.0:PORT,
// letting anything on the pod network POST into Claude's channel. Refuse to start.
if (!TOKEN) {
  console.error(
    `gardenio-channel: FATAL — neither GARDENIO_CHANNEL_TOKEN nor COOP_BROKER_TOKEN ` +
      `is set. Refusing to start HTTP listener on 0.0.0.0:${PORT} without authentication.`,
  );
  process.exit(1);
}

// Precompute a SHA-256 digest of the expected token so authorize() can use
// timingSafeEqual on fixed-length buffers regardless of supplied length.
const TOKEN_DIGEST = createHash("sha256").update(TOKEN).digest();

function authorize(req: Request): boolean {
  const header = req.headers.get("Authorization");
  if (!header || !header.startsWith("Bearer ")) return false;
  const supplied = header.slice("Bearer ".length);
  const suppliedDigest = createHash("sha256").update(supplied).digest();
  return timingSafeEqual(TOKEN_DIGEST, suppliedDigest);
}

// ── MCP Server ─────────────────────────────────────────────────────────

const mcp = new Server(
  { name: "gardenio", version: "0.0.1" },
  {
    capabilities: {
      experimental: {
        "claude/channel": {},
        "claude/channel/permission": {},
      },
      tools: {},
    },
    instructions: [
      'Messages from the gardenio orchestrator arrive as <channel source="gardenio" ...> tags.',
      "These are nudges, task assignments, mail, decision resolutions, or review comments",
      "from the bridge/controller. Treat them as high-priority operator messages.",
      "If the message contains a bead ID (ka-XXXXX), run `kd show <id>` to get full context.",
      "Use the gardenio__reply tool to send a brief acknowledgment or status update back.",
    ].join(" "),
  }
);

// ── Reply tool (two-way) ───────────────────────────────────────────────

mcp.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: "reply",
      description:
        "Send a status update or acknowledgment back to the gardenio orchestrator. " +
        "Use this to confirm you received a nudge, report progress, or flag blockers.",
      inputSchema: {
        type: "object" as const,
        properties: {
          text: {
            type: "string",
            description: "The message to send back",
          },
          bead_id: {
            type: "string",
            description: "Optional bead ID this reply relates to",
          },
        },
        required: ["text"],
      },
    },
  ],
}));

mcp.setRequestHandler(CallToolRequestSchema, async (req) => {
  if (req.params.name === "reply") {
    const { text, bead_id } = req.params.arguments as {
      text: string;
      bead_id?: string;
    };

    if (CALLBACK_URL) {
      try {
        const headers: Record<string, string> = {
          "Content-Type": "application/json",
        };
        if (TOKEN) headers["Authorization"] = `Bearer ${TOKEN}`;

        await fetch(CALLBACK_URL, {
          method: "POST",
          headers,
          body: JSON.stringify({ type: "reply", text, bead_id }),
        });
      } catch {
        // Best-effort delivery — don't fail Claude's tool call.
      }
    }

    return { content: [{ type: "text" as const, text: "sent" }] };
  }
  throw new Error(`unknown tool: ${req.params.name}`);
});

// ── Permission relay ───────────────────────────────────────────────────
//
// Claude Code sends notifications/claude/channel/permission_request when
// a tool-use dialog opens. We forward the prompt to the bridge (which
// shows it in Slack). The operator's verdict comes back via POST /permission
// on our HTTP listener.

const PermissionRequestSchema = z.object({
  method: z.literal("notifications/claude/channel/permission_request"),
  params: z.object({
    request_id: z.string(),
    tool_name: z.string(),
    description: z.string(),
    input_preview: z.string(),
  }),
});

mcp.setNotificationHandler(PermissionRequestSchema, async ({ params }) => {
  if (!CALLBACK_URL) return;

  try {
    // The bridge takes the agent from our per-agent token (the body's agent
    // must match it). channel_url is ignored by the bridge, which resolves the
    // verdict URL from the agent bead; it stays for schema compatibility.
    const podIP = process.env.POD_IP || "127.0.0.1";
    const channelURL = `http://${podIP}:${PORT}`;
    const body = JSON.stringify({
      type: "permission_request",
      request_id: params.request_id,
      tool_name: params.tool_name,
      description: params.description,
      input_preview: params.input_preview,
      agent: AGENT_NAME,
      channel_url: channelURL,
    });

    const send = () => {
      const headers: Record<string, string> = {
        "Content-Type": "application/json",
      };
      if (TOKEN) headers["Authorization"] = `Bearer ${TOKEN}`;
      if (agentToken) headers[AGENT_TOKEN_HEADER] = agentToken;
      return fetch(CALLBACK_URL, { method: "POST", headers, body });
    };
    const resp = await send();
    if (resp.status === 401 && agentToken && (await refreshAgentToken())) {
      await send();
    }
  } catch {
    // Best-effort — if the bridge is unreachable the local terminal
    // dialog is still available.
  }
});

// ── Connect to Claude Code ─────────────────────────────────────────────

await mcp.connect(new StdioServerTransport());

// ── HTTP listener for bridge messages ──────────────────────────────────

Bun.serve({
  port: PORT,
  hostname: "0.0.0.0",
  async fetch(req) {
    // Auth check. TOKEN is guaranteed non-empty (fail-closed at startup).
    if (!authorize(req)) {
      return new Response("unauthorized", { status: 401 });
    }

    if (req.method !== "POST") {
      return new Response("method not allowed", { status: 405 });
    }

    const url = new URL(req.url);

    // POST /permission — verdict from the bridge (Slack operator).
    if (url.pathname === "/permission") {
      try {
        const body = await req.json() as {
          request_id?: string;
          behavior?: string;
        };
        const request_id = body.request_id ?? "";
        const behavior = body.behavior ?? "deny";

        if (!request_id) {
          return new Response(
            JSON.stringify({ error: "missing request_id" }),
            { status: 400, headers: { "Content-Type": "application/json" } }
          );
        }
        if (behavior !== "allow" && behavior !== "deny") {
          return new Response(
            JSON.stringify({ error: "behavior must be allow or deny" }),
            { status: 400, headers: { "Content-Type": "application/json" } }
          );
        }

        await mcp.notification({
          method: "notifications/claude/channel/permission",
          params: { request_id, behavior },
        });

        return new Response(
          JSON.stringify({ delivered: true }),
          { headers: { "Content-Type": "application/json" } }
        );
      } catch (err) {
        const msg = err instanceof Error ? err.message : "unknown error";
        return new Response(
          JSON.stringify({ delivered: false, reason: msg }),
          { status: 500, headers: { "Content-Type": "application/json" } }
        );
      }
    }

    // POST / — channel event from the bridge (nudges, mail, etc.).
    try {
      const body = await req.text();

      // Try to parse as JSON for structured messages.
      let content = body;
      let meta: Record<string, string> = {};

      try {
        const parsed = JSON.parse(body);
        if (typeof parsed === "object" && parsed !== null) {
          content = parsed.message || parsed.content || body;
          if (parsed.bead_id) meta.bead_id = String(parsed.bead_id);
          if (parsed.type) meta.type = String(parsed.type);
          if (parsed.source) meta.source = String(parsed.source);
          if (parsed.agent) meta.agent = String(parsed.agent);
          if (parsed.thread_ts) meta.thread_ts = String(parsed.thread_ts);
        }
      } catch {
        // Plain text message — use as-is.
      }

      await mcp.notification({
        method: "notifications/claude/channel",
        params: { content, meta },
      });

      return new Response(JSON.stringify({ delivered: true }), {
        headers: { "Content-Type": "application/json" },
      });
    } catch (err) {
      const msg = err instanceof Error ? err.message : "unknown error";
      return new Response(
        JSON.stringify({ delivered: false, reason: msg }),
        { status: 500, headers: { "Content-Type": "application/json" } }
      );
    }
  },
});
