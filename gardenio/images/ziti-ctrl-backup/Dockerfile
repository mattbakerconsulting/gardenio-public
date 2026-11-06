# ziti-ctrl-backup -- amazon/aws-cli plus tar, gzip and openssl, baked in at
# BUILD time.
#
# Why this image exists (gd-fY6ipET226, second attempt): the k3s
# ziti-controller's durable-storage design runs an initContainer and a sidecar
# that tar + gzip + openssl-encrypt the controller's ZITI_HOME and push/pull it
# to S3. The first attempt reused plain `amazon/aws-cli:2.36.48` (the image
# helm/beads/dolt-backup-cronjob.yaml already uses) and was applied, found
# broken, and reverted: that image ships sha256sum, awk, find, cp, dd and yum,
# but NO tar, gzip or openssl. Both the backup and the restore sidecar need all
# three; s3-backup died "tar: command not found" (exit 127) and crash-looped,
# and restore-if-empty would have failed identically the first time it had
# something to actually restore. See commit 0f158ff for the full incident
# writeup.
#
# Installing tar/gzip/openssl at pod-BOOT time with yum was considered and
# rejected: that puts a package mirror on the controller's critical restart
# path, a bad trade for a control plane that needs to come back fast after a
# node replacement. Baking them into the image at CI build time avoids that
# entirely -- the yum transaction happens once, here, not on every pod start.
#
# Pin the upstream tag alongside dolt-backup-cronjob.yaml's `amazon/aws-cli`
# pin so the two stay easy to compare; bump both deliberately together if this
# base is ever moved off.
FROM amazon/aws-cli:2.36.48

RUN yum install -y tar gzip openssl && yum clean all && rm -rf /var/cache/yum

# Fail the build, not a pod three weeks from now, if a future base image drops
# one of these again.
RUN aws --version && tar --version | head -1 && gzip --version | head -1 && openssl version
