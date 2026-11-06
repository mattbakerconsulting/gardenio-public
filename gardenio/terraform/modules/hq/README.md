# modules/hq

One on-demand EC2 host for the Gas City HQ supervisor (placement A: public
subnet, public IP, zero-ingress security group; ziti and SSM are outbound-only
dials). Provisions itself from `files/hq-bootstrap.sh` on first boot and powers
itself off when idle. Operator guide, cost math and the scale-down design:
`gardenio/docs/hq-host.md`. Plan: `hq/plans/hq-on-aws.md` (private mirror).

Composed in `composition/gardenio/main.tf` as `module "hq"`; helper CLI at
`gardenio/scripts/hq`.
