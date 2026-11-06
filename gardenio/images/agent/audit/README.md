# Agent image dynamic-scan outputs

Raw outputs from `ka-0000008201` (follow-up to audit ka-0000000248).
Summarised in `../AUDIT.md` under "Trivy / dive / SUID — dynamic checks".

Re-run:

    IMG=<registry>/gardenio/agent:<tag>
    docker run --rm --network host -e DOCKER_HOST="$DOCKER_HOST" \
      aquasec/trivy:latest image --severity HIGH,CRITICAL --ignore-unfixed \
      --format table "$IMG" > trivy-<tag>.txt
    docker run --rm --network host -e DOCKER_HOST="$DOCKER_HOST" -e CI=true \
      wagoodman/dive:latest "$IMG" --ci > dive-<tag>.txt
    docker run --rm --entrypoint find "$IMG" / -xdev -perm /6000 -type f \
      > suid-<tag>.txt
