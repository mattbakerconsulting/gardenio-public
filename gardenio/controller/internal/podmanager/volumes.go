package podmanager

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// buildVolumes returns the volumes for the pod based on role.
func (m *K8sManager) buildVolumes(spec AgentPodSpec) []corev1.Volume {
	var volumes []corev1.Volume

	// Workspace volume: PVC for persistent roles, EmptyDir for ephemeral.
	if spec.WorkspaceStorage != nil {
		claimName := spec.WorkspaceStorage.ClaimName
		if claimName == "" {
			claimName = spec.PodName() + "-workspace"
		}
		volumes = append(volumes, corev1.Volume{
			Name: VolumeWorkspace,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claimName,
				},
			},
		})
	} else {
		volumes = append(volumes, corev1.Volume{
			Name: VolumeWorkspace,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}

	// Tmp volume: always EmptyDir.
	volumes = append(volumes, corev1.Volume{
		Name: VolumeTmp,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	// Beads config volume: ConfigMap mount if specified.
	if spec.ConfigMapName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: VolumeBeadsConfig,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: spec.ConfigMapName},
				},
			},
		})
	}

	// Docker-in-Docker storage volume.
	if spec.Docker {
		volumes = append(volumes, corev1.Volume{
			Name: VolumeDindStorage,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}

	// PostgreSQL data volume.
	if spec.Postgres {
		volumes = append(volumes, corev1.Volume{
			Name: VolumePostgresData,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}

	// Claude credentials volume: Secret mount for OAuth token.
	if spec.CredentialsSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: VolumeClaudeCreds,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: spec.CredentialsSecret,
				},
			},
		})
	}

	// NATS token volume (W5, ka-0000006585): project the "token" key from
	// the NATS agent creds Secret as a single file at
	// MountNatsToken/NatsTokenFileName with mode 0400. K8s Secret volumes
	// are tmpfs-backed by default, so the token never touches node disk
	// and is not visible via /proc/self/environ or `kubectl describe pod`.
	if spec.NatsTokenSecret != "" {
		mode := NatsTokenFileMode
		volumes = append(volumes, corev1.Volume{
			Name: VolumeNatsToken,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  spec.NatsTokenSecret,
					DefaultMode: &mode,
					Items: []corev1.KeyToPath{
						{Key: "token", Path: NatsTokenFileName, Mode: &mode},
					},
				},
			},
		})
	}

	// Object-store credentials volume (gd-W8OwRoHLLn): project the
	// "accesskey"/"secretkey" keys from the MinIO Secret as two files under
	// MountObjectStoreCreds, mode 0400. Endpoint mode only — mirrors the NATS
	// token pattern so the access/secret key never touch node disk or appear
	// via /proc/self/environ or `kubectl describe pod`.
	if spec.ObjectStoreCredentialsSecret != "" {
		mode := ObjectStoreCredFileMode
		volumes = append(volumes, corev1.Volume{
			Name: VolumeObjectStoreCreds,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  spec.ObjectStoreCredentialsSecret,
					DefaultMode: &mode,
					Items: []corev1.KeyToPath{
						{Key: "accesskey", Path: ObjectStoreAccessKeyFileName, Mode: &mode},
						{Key: "secretkey", Path: ObjectStoreSecretKeyFileName, Mode: &mode},
					},
				},
			},
		})
	}

	// Git credentials volume: projected Secret that materializes git creds as
	// files under /etc/git-creds/ in the init-clone container. The init-clone
	// script reads these files (instead of consuming GIT_USERNAME/GIT_TOKEN
	// env vars) so credentials do not leak via process listings, kubectl
	// describe, or admin log dumps. See W6 (ka-0000002787).
	if spec.GitCredentialsSecret != "" || spec.GitlabTokenSecret != "" {
		mode := GitCredFileMode
		var sources []corev1.VolumeProjection
		if spec.GitCredentialsSecret != "" {
			sources = append(sources, corev1.VolumeProjection{
				Secret: &corev1.SecretProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: spec.GitCredentialsSecret},
					Items: []corev1.KeyToPath{
						{Key: "username", Path: "github/username", Mode: &mode},
						{Key: "token", Path: "github/token", Mode: &mode},
					},
				},
			})
		}
		if spec.GitlabTokenSecret != "" {
			sources = append(sources, corev1.VolumeProjection{
				Secret: &corev1.SecretProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: spec.GitlabTokenSecret},
					Items: []corev1.KeyToPath{
						{Key: "token", Path: "gitlab/token", Mode: &mode},
					},
				},
			})
		}
		volumes = append(volumes, corev1.Volume{
			Name: VolumeGitCreds,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					DefaultMode: &mode,
					Sources:     sources,
				},
			},
		})
	}

	// Codex subscription login (provider=openai): the derived -session Secret,
	// projected as the single auth.json file.
	if spec.CodexAuthSecret != "" {
		mode := CodexAuthFileMode
		volumes = append(volumes, corev1.Volume{
			Name: VolumeCodexAuth,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  spec.CodexAuthSecret,
					DefaultMode: &mode,
					Items: []corev1.KeyToPath{
						{Key: CodexAuthFileName, Path: CodexAuthFileName, Mode: &mode},
					},
				},
			},
		})
	}

	// Gemini Google-account login: the single oauth_creds.json file.
	if spec.GeminiOAuthSecret != "" {
		mode := CodexAuthFileMode
		volumes = append(volumes, corev1.Volume{
			Name: VolumeGeminiAuth,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  spec.GeminiOAuthSecret,
					DefaultMode: &mode,
					Items: []corev1.KeyToPath{
						{Key: GeminiOAuthFileName, Path: GeminiOAuthFileName, Mode: &mode},
					},
				},
			},
		})
	}

	// Slack viewer session: optional, so an unprovisioned Secret leaves the
	// mount empty instead of wedging the pod in ContainerCreating.
	if spec.SlackViewSessionSecret != "" {
		mode := SlackViewSessionFileMode
		optional := true
		volumes = append(volumes, corev1.Volume{
			Name: VolumeSlackViewSession,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  spec.SlackViewSessionSecret,
					DefaultMode: &mode,
					Optional:    &optional,
					Items: []corev1.KeyToPath{
						{Key: SlackViewSessionFileName, Path: SlackViewSessionFileName, Mode: &mode},
					},
				},
			},
		})
	}

	// Machine client secret: the one key, as a 0400 file.
	if spec.MachineClientSecret != "" {
		mode := MachineClientFileMode
		volumes = append(volumes, corev1.Volume{
			Name: VolumeMachineClient,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  spec.MachineClientSecret,
					DefaultMode: &mode,
					Items: []corev1.KeyToPath{
						{Key: MachineClientSecretKey, Path: MachineClientSecretFileName, Mode: &mode},
					},
				},
			},
		})
	}

	volumes = append(volumes, machineClientCAVolumes(spec)...)

	// SSH key volumes: one per spec.SshKeys entry, each projecting a single
	// Secret key as a file with mode 0400 under /etc/ssh-keys/<name>. Iterate
	// in sorted key order so volume ordering is deterministic across calls.
	for _, name := range sortedSshKeyNames(spec.SshKeys) {
		src := spec.SshKeys[name]
		mode := SshKeyFileMode
		volumes = append(volumes, corev1.Volume{
			Name: VolumeSshKeyPrefix + name,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  src.SecretName,
					DefaultMode: &mode,
					Items: []corev1.KeyToPath{
						{Key: src.SecretKey, Path: name, Mode: &mode},
					},
				},
			},
		})
	}

	// Runtime coverage instrumentation (C2, ka-0000000508): mount the shared
	// coverage PVC so GOCOVERDIR/LLVM_PROFILE_FILE (set in buildEnvVars) have a
	// real destination. Same ReadWriteMany claim the daemons mount, so the e2e
	// drain task collects every pod's counters from one PVC. No-op unless
	// coverage is active.
	if m.Coverage.active() && !spec.LandFenced {
		volumes = append(volumes, corev1.Volume{
			Name: VolumeCoverageData,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: m.Coverage.ClaimName,
				},
			},
		})
	}

	// OpenZiti per-pod overlay access (epic ka-0000000990): projected
	// broker-audience token + Memory-backed identity emptyDir + host tun
	// device. No-op when spec.ZitiAccess is nil.
	volumes = append(volumes, m.buildZitiVolumes(spec)...)

	// gVisor thaw-epoch rendezvous (rehydration-design §3.1, ka-0000004017).
	// Only runsc-pinned pods participate in freeze/thaw, so only they carry the
	// per-agent hostPath that the controller (over NodeExecutor) and the in-pod
	// gb+coop thaw watchers share. A non-runsc pod never gets a hostPath. The
	// placeholder Pod B inherits this volume via the snapshot's PodTemplate
	// deep-copy, so the thawed pod sees the same node directory. We use
	// DirectoryOrCreate so the kubelet provisions the dir on first thaw before
	// the controller's marker writer chowns it to the agent UID.
	if spec.RuntimeClassName == RuntimeClassRunsc && !spec.NoThawState {
		hostType := corev1.HostPathDirectoryOrCreate
		volumes = append(volumes, corev1.Volume{
			Name: VolumeThawState,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: thawStateHostDir(spec.AgentName),
					Type: &hostType,
				},
			},
		})
	}

	return volumes
}

// thawStateHostDir is the per-agent host directory the thaw-state hostPath
// volume sources from. Joined under ThawStateHostBase and keyed by a sanitized
// agent name so concurrent agents on the same node never share a marker dir.
// It MUST match the path the controller's NodeRehydrationTrigger writes the
// marker into (freezethaw_rehydrate.go), so both sides address the same inode.
func thawStateHostDir(agentName string) string {
	return ThawStateHostBase + "/" + sanitizeThawAgentDir(agentName)
}

// sanitizeThawAgentDir reduces an agent name to a filesystem-safe single path
// segment (alnum, dash, underscore, dot), so it is safe to interpolate into a
// host path and a shell command. Anything else becomes '_'. Empty input yields
// "agent" so the path is never the bare base dir.
func sanitizeThawAgentDir(agentName string) string {
	if agentName == "" {
		return "agent"
	}
	out := make([]rune, 0, len(agentName))
	for _, r := range agentName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// sortedSshKeyNames returns the map keys in deterministic order so volume
// and mount lists are stable across calls.
func sortedSshKeyNames(m map[string]SshKeySource) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// buildVolumeMounts returns the volume mounts for the agent container.
func (m *K8sManager) buildVolumeMounts(spec AgentPodSpec) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: VolumeWorkspace, MountPath: MountWorkspace},
		{Name: VolumeTmp, MountPath: MountTmp},
	}

	if spec.ConfigMapName != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeBeadsConfig,
			MountPath: MountBeadsConfig,
			ReadOnly:  true,
		})
	}

	// Claude state: mount workspace subPath at ~/.claude so Claude Code's
	// session data lives on the workspace volume. S3 handles persistence
	// across pod restarts; this mount ensures consistent paths during the
	// pod lifecycle regardless of storage type (PVC or EmptyDir).
	// harness=codex mounts ~/.codex (CODEX_HOME) and harness=gemini ~/.gemini
	// instead; see HarnessStateMount.
	statePath, stateSubPath := HarnessStateMount(spec.Harness)
	mounts = append(mounts, corev1.VolumeMount{
		Name:      VolumeWorkspace,
		MountPath: statePath,
		SubPath:   stateSubPath,
	})

	// Claude credentials: mount secret to staging dir; entrypoint/gb copies to PVC.
	if spec.CredentialsSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeClaudeCreds,
			MountPath: MountClaudeCreds,
			ReadOnly:  true,
		})
	}

	// NATS token: tmpfs-backed Secret volume read by coop / gb hook-relay
	// via $COOP_NATS_TOKEN_FILE (see manager.MountNatsToken).
	if spec.NatsTokenSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeNatsToken,
			MountPath: MountNatsToken,
			ReadOnly:  true,
		})
	}

	// Object-store credentials: tmpfs-backed Secret volume read by coop via
	// $COOP_S3_ACCESS_KEY_ID_FILE / $COOP_S3_SECRET_ACCESS_KEY_FILE (see
	// manager.MountObjectStoreCreds). Endpoint mode only.
	if spec.ObjectStoreCredentialsSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeObjectStoreCreds,
			MountPath: MountObjectStoreCreds,
			ReadOnly:  true,
		})
	}

	// Git credentials: same projected /etc/git-creds mount the init container
	// uses (W6, ka-0000002787), also given to the agent container so `gb
	// agent start` can authenticate `gh` from the file rather than adding a
	// GH_TOKEN env var (gd-koIbfse9OE). Read-only; tmpfs-backed Secret
	// projection, never touches node disk.
	if spec.GitCredentialsSecret != "" || spec.GitlabTokenSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeGitCreds,
			MountPath: MountGitCreds,
			ReadOnly:  true,
		})
	}

	// Codex login: read-only directory mount. The pod start step copies
	// auth.json to the writable $CODEX_HOME; this mount is never written.
	if spec.CodexAuthSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeCodexAuth,
			MountPath: MountCodexAuth,
			ReadOnly:  true,
		})
	}

	// Gemini login: read-only directory mount; `gb setup gemini` copies the file
	// to the writable ~/.gemini because the CLI rewrites it on token refresh.
	if spec.GeminiOAuthSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeGeminiAuth,
			MountPath: MountGeminiAuth,
			ReadOnly:  true,
		})
	}

	// Slack viewer session: read-only directory mount (empty when unprovisioned).
	if spec.SlackViewSessionSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeSlackViewSession,
			MountPath: MountSlackViewSession,
			ReadOnly:  true,
		})
	}

	// Machine client secret: read-only, for kd and gb --as-machine.
	if spec.MachineClientSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeMachineClient,
			MountPath: MountMachineClient,
			ReadOnly:  true,
		})
		mounts = append(mounts, machineClientCAMounts(spec)...)
	}

	// SSH keys: mount each projected Secret as a single file at
	// /etc/ssh-keys/<name>. SubPath limits the mount to one file so multiple
	// keys from different Secrets can coexist under the same parent dir.
	for _, name := range sortedSshKeyNames(spec.SshKeys) {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeSshKeyPrefix + name,
			MountPath: MountSshKeys + "/" + name,
			SubPath:   name,
			ReadOnly:  true,
		})
	}

	// gVisor thaw-epoch rendezvous (ka-0000004017): mount the per-agent
	// hostPath at MountThawState so the in-pod gb+coop watchers read the marker
	// and write acks at the same inode the controller addresses on the node.
	// Read-write because the agent writes its ack files here. Only on runsc
	// pods (the volume is only added there).
	if spec.RuntimeClassName == RuntimeClassRunsc && !spec.NoThawState {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeThawState,
			MountPath: MountThawState,
		})
	}

	// OpenZiti per-pod access (epic ka-0000000990): the agent container gets
	// read-only views of its broker token (e.g. to self-revoke) and the
	// enrolled identity dir. No-op when spec.ZitiAccess is nil.
	mounts = append(mounts, m.buildZitiAgentMounts(spec)...)

	// Runtime coverage instrumentation (C2, ka-0000000508): mount the shared
	// coverage PVC read-write at MountPath so GOCOVERDIR/LLVM_PROFILE_FILE (set
	// in buildEnvVars) resolve to real files. No-op unless coverage is active;
	// paired with the coverage-data volume added in buildVolumes.
	if m.Coverage.active() && !spec.LandFenced {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      VolumeCoverageData,
			MountPath: m.Coverage.mountPath(),
		})
	}

	return mounts
}

// HarnessStateMount returns the in-pod mount path and workspace-volume subPath
// that back the agent CLI's state directory: ~/.codex for the codex harness,
// ~/.gemini for the gemini harness, ~/.claude for everything else (including
// the empty/default harness), so a pod that never set a harness is unchanged.
func HarnessStateMount(harness string) (mountPath, subPath string) {
	switch harness {
	case "codex":
		return MountCodexState, SubPathCodexState
	case "gemini":
		return MountGeminiState, SubPathGeminiState
	}
	return MountClaudeState, SubPathClaudeState
}

// machineClientCAVolumes is the optional keycloak-runtime ca.crt volume of a
// machine-client pod (gd-pnrrLJNUHn), or none.
func machineClientCAVolumes(spec AgentPodSpec) []corev1.Volume {
	if spec.MachineClientSecret == "" || spec.MachineClientCASecret == "" {
		return nil
	}
	mode := MachineClientCAFileMode
	optional := true
	return []corev1.Volume{{
		Name: VolumeMachineClientCA,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  spec.MachineClientCASecret,
				DefaultMode: &mode,
				Optional:    &optional,
				Items:       []corev1.KeyToPath{{Key: MachineClientCAFileName, Path: MachineClientCAFileName}},
			},
		},
	}}
}

// machineClientCAMounts mounts machineClientCAVolumes at MountMachineClientCA.
func machineClientCAMounts(spec AgentPodSpec) []corev1.VolumeMount {
	if spec.MachineClientSecret == "" || spec.MachineClientCASecret == "" {
		return nil
	}
	return []corev1.VolumeMount{{Name: VolumeMachineClientCA, MountPath: MountMachineClientCA, ReadOnly: true}}
}
