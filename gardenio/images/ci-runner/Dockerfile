# ci-runner — lightweight image with deploy/E2E tools pre-installed.
#
# Was the base image for the RWX deploy and e2e pipelines (deleted 2026-09-16),
# so they skipped tool installation. No workflow builds it today (images.yml
# does not list it).
#
# Tools included: helm, kubectl, AWS CLI v2, Go, jq, curl, unzip, python3
#
# Build:
#   docker build -t ghcr.io/mattbakerconsulting/gardenio/ci-runner:latest gardenio/images/ci-runner/

FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl unzip jq python3 ca-certificates git sudo \
    && rm -rf /var/lib/apt/lists/*

# Helm 3
RUN curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# kubectl (pin to latest stable at build time)
RUN KUBE_VERSION=$(curl -sSfL https://dl.k8s.io/release/stable.txt) && \
    curl -sSfL "https://dl.k8s.io/release/${KUBE_VERSION}/bin/linux/amd64/kubectl" \
      -o /usr/local/bin/kubectl && \
    chmod +x /usr/local/bin/kubectl

# AWS CLI v2
RUN curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o /tmp/awscliv2.zip && \
    cd /tmp && unzip -q awscliv2.zip && ./aws/install && \
    rm -rf /tmp/awscliv2.zip /tmp/aws

# Go 1.25 (for building gb/kd from source in E2E pipeline)
RUN curl -fsSL "https://go.dev/dl/go1.25.9.linux-amd64.tar.gz" | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"

# crane (for image operations)
RUN curl -fsSL "https://github.com/google/go-containerregistry/releases/download/v0.20.2/go-containerregistry_Linux_x86_64.tar.gz" \
      | tar xz -C /usr/local/bin crane

# Verify all tools are present
RUN helm version --short && \
    kubectl version --client --short 2>/dev/null || kubectl version --client && \
    aws --version && \
    go version && \
    crane version && \
    echo "All ci-runner tools verified"
