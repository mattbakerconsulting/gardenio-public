#!/bin/sh
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
#
# Pod-creation script for coopmux running in Kubernetes.
# Called by coopmux when POST /api/v1/sessions/launch fires.
#
# Expected env (set by coopmux launch handler):
#   COOP_MUX_URL              - mux URL (ignored; we use Service DNS instead)
#   COOP_MUX_TOKEN            - auth token for session registration
#   ANTHROPIC_API_KEY         - credential from broker (if healthy)
#   CLAUDE_CODE_OAUTH_TOKEN   - credential from broker (if healthy)
#   GIT_REPO                  - git repository URL (optional, for git clone)
#   GIT_BRANCH                - git branch to checkout (optional, default: main)
#   WORKING_DIR               - working directory for session (optional, default: /workspace)
#
# Expected env (set in coopmux pod spec):
#   POD_NAMESPACE        - namespace (downward API)
#   COOP_SESSION_IMAGE   - image for session pods (default: coop:claude)

set -eu

SESSION_ID=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || uuidgen)
SHORT_ID=$(echo "$SESSION_ID" | cut -c1-8)
POD_NAME="coop-session-${SHORT_ID}"
NAMESPACE="${POD_NAMESPACE:-coop}"
IMAGE="${COOP_SESSION_IMAGE:-coop:claude}"
MUX_URL="http://coopmux.${NAMESPACE}.svc.cluster.local:9800"
MUX_TOKEN="${COOP_MUX_TOKEN:-}"

# Optional user-supplied env vars (passed from launch dialog).
GIT_REPO="${GIT_REPO:-}"
GIT_BRANCH="${GIT_BRANCH:-main}"
WORKING_DIR="${WORKING_DIR:-/workspace}"

# Build credential env entries. Prefer values injected by the mux broker;
# fall back to the k8s secret if no broker value is available.
CRED_ENV=""
if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
  CRED_ENV="${CRED_ENV}
        - name: ANTHROPIC_API_KEY
          value: \"${ANTHROPIC_API_KEY}\""
else
  CRED_ENV="${CRED_ENV}
        - name: ANTHROPIC_API_KEY
          valueFrom:
            secretKeyRef:
              name: anthropic-credentials
              key: api-key
              optional: true"
fi

if [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
  CRED_ENV="${CRED_ENV}
        - name: CLAUDE_CODE_OAUTH_TOKEN
          value: \"${CLAUDE_CODE_OAUTH_TOKEN}\""
else
  CRED_ENV="${CRED_ENV}
        - name: CLAUDE_CODE_OAUTH_TOKEN
          valueFrom:
            secretKeyRef:
              name: anthropic-credentials
              key: oauth-token
              optional: true"
fi

# Build init container spec if GIT_REPO is set.
INIT_CONTAINERS=""
if [ -n "$GIT_REPO" ]; then
  INIT_CONTAINERS="  initContainers:
    - name: git-clone
      image: alpine/git:latest
      workingDir: /workspace
      command: [\"sh\", \"-c\"]
      args:
        - |
          set -e
          echo \"Cloning ${GIT_REPO} (branch: ${GIT_BRANCH})...\"
          # Blobless partial clone (not shallow): downloads full commit+tree
          # history and all branch/tag names but skips file contents (blobs),
          # backfilling them on-demand. Gives working git merge-base/ancestry
          # for ~the shallow price and avoids the 'no common history' confusion
          # a --depth 1 shallow clone causes. See epic ka-0000005334.
          git clone --filter=blob:none --branch \"${GIT_BRANCH}\" \"${GIT_REPO}\" repo
          echo \"Clone complete\"
      volumeMounts:
        - name: workspace
          mountPath: /workspace"
fi

kubectl apply -n "$NAMESPACE" -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: coop-session
spec:
  serviceAccountName: coop-session
  restartPolicy: Never
${INIT_CONTAINERS}
  containers:
    - name: coop
      image: ${IMAGE}
      imagePullPolicy: Never
      workingDir: ${WORKING_DIR}
      command: ["sh", "-c"]
      args:
        - |
          # Write minimal Claude config so the CLI skips onboarding when
          # credentials are supplied via env vars.
          if [ -n "\${CLAUDE_CODE_OAUTH_TOKEN:-}" ] || [ -n "\${ANTHROPIC_API_KEY:-}" ]; then
            VER=\$(claude --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
            VER=\${VER:-0.0.0}
            CWD=\$(pwd)
            printf '{"hasCompletedOnboarding":true,"lastOnboardingVersion":"%s","projects":{"%s":{"hasTrustDialogAccepted":true,"allowedTools":[]}}}\n' "\$VER" "\$CWD" > "\$HOME/.claude.json"
          fi
          export COOP_URL="http://\${POD_IP}:8080"
          exec coop --host 0.0.0.0 --port 8080 --log-format text -- claude
      env:
        - name: COOP_MUX_URL
          value: "${MUX_URL}"
        - name: COOP_MUX_TOKEN
          value: "${MUX_TOKEN}"
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        - name: POD_IP
          valueFrom:
            fieldRef:
              fieldPath: status.podIP${CRED_ENV}
      ports:
        - containerPort: 8080
      volumeMounts:
        - name: workspace
          mountPath: /workspace
      livenessProbe:
        httpGet:
          path: /api/v1/health
          port: 8080
        initialDelaySeconds: 5
        periodSeconds: 10
      readinessProbe:
        httpGet:
          path: /api/v1/health
          port: 8080
        initialDelaySeconds: 2
        periodSeconds: 5
  volumes:
    - name: workspace
      emptyDir: {}
EOF

echo "Created session pod: ${POD_NAME}"
