variable "env_name" { type = string }
variable "vpc_id" { type = string }
variable "subnet_id" {
  type        = string
  description = "Public subnet for placement A: public IP, zero-ingress SG, ziti/SSM are outbound dials."
}
variable "ami_id" {
  type        = string
  description = "Ubuntu 24.04 AMI. Ignored after first create (see lifecycle) so a newer SSM AMI never replaces the host."
}
variable "instance_type" {
  type        = string
  default     = "m6a.large"
  description = "Resize = change this + apply (terraform stops/starts the instance itself), or scripts/hq resize."
}
variable "root_volume_gb" {
  type    = number
  default = 40
}
variable "snapshot_retain_count" {
  type    = number
  default = 7
}
variable "gc_user" {
  type    = string
  default = "gc"
}
variable "ziti_identity_name" {
  type    = string
  default = "gardenio-hq"
}

# ── Toolchain pins baked into /etc/hq/pins.env ───────────────────────────
variable "go_version" {
  type        = string
  default     = "1.26.6" # upstream go.mod `go 1.26.6` at gc_commit
  description = "Go release for building gc on the host (must satisfy upstream go.mod)."
}
variable "node_major" {
  type    = string
  default = "22" # .github/actions/setup-gascity-ubuntu default
}
variable "dolt_version" {
  type    = string
  default = "2.1.7" # fork-verify.yml DOLT_VERSION; matches the Vultr city
}
variable "bd_version" {
  type    = string
  default = "v1.3.0-rc.2" # fork-verify.yml BD_VERSION
}
variable "gc_commit" {
  type        = string
  default     = "d807c8e0dd6d071a79e821be5da73097dbf4ff96"
  description = "Upstream gastownhall/gascity commit gc is built from (go build ./cmd/gc)."
}
variable "gc_repo_url" {
  type    = string
  default = "https://github.com/gastownhall/gascity.git"
}
variable "ziti_tunnel_version" {
  type    = string
  default = "v1.18.7" # openziti/ziti-tunnel-sdk-c latest release, 2026-09-17
}
variable "common_tags" {
  type    = map(string)
  default = {}
}
