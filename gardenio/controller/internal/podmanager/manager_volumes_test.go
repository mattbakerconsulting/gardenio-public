package podmanager

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// --- buildVolumes tests ---

func TestBuildVolumes_EmptyDir(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	volumes := m.buildVolumes(spec)

	volMap := make(map[string]corev1.Volume)
	for _, v := range volumes {
		volMap[v.Name] = v
	}

	ws, ok := volMap[VolumeWorkspace]
	if !ok {
		t.Fatal("missing workspace volume")
	}
	if ws.VolumeSource.EmptyDir == nil {
		t.Error("workspace should be EmptyDir without WorkspaceStorage")
	}

	tmp, ok := volMap[VolumeTmp]
	if !ok {
		t.Fatal("missing tmp volume")
	}
	if tmp.VolumeSource.EmptyDir == nil {
		t.Error("tmp should be EmptyDir")
	}
}

func TestBuildVolumes_PVC(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.WorkspaceStorage = &WorkspaceStorageSpec{ClaimName: "my-pvc"}
	volumes := m.buildVolumes(spec)

	for _, v := range volumes {
		if v.Name == VolumeWorkspace {
			if v.VolumeSource.PersistentVolumeClaim == nil {
				t.Fatal("workspace should be PVC with WorkspaceStorage")
			}
			if v.VolumeSource.PersistentVolumeClaim.ClaimName != "my-pvc" {
				t.Errorf("PVC claim = %q, want my-pvc",
					v.VolumeSource.PersistentVolumeClaim.ClaimName)
			}
			return
		}
	}
	t.Error("workspace volume not found")
}

func TestBuildVolumes_PVCDefaultName(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.WorkspaceStorage = &WorkspaceStorageSpec{} // empty ClaimName
	volumes := m.buildVolumes(spec)

	for _, v := range volumes {
		if v.Name == VolumeWorkspace {
			want := spec.PodName() + "-workspace"
			if v.VolumeSource.PersistentVolumeClaim.ClaimName != want {
				t.Errorf("PVC claim = %q, want %q",
					v.VolumeSource.PersistentVolumeClaim.ClaimName, want)
			}
			return
		}
	}
	t.Error("workspace volume not found")
}

func TestBuildVolumes_TmpAlwaysPresent(t *testing.T) {
	mgr := New(fake.NewSimpleClientset(), testLogger())
	spec := AgentPodSpec{}

	volumes := mgr.buildVolumes(spec)

	var found bool
	for _, v := range volumes {
		if v.Name == VolumeTmp && v.EmptyDir != nil {
			found = true
		}
	}
	if !found {
		t.Error("expected tmp EmptyDir volume")
	}
}

func TestBuildVolumes_ConfigMap(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.ConfigMapName = "agent-config"
	volumes := m.buildVolumes(spec)

	for _, v := range volumes {
		if v.Name == VolumeBeadsConfig {
			if v.VolumeSource.ConfigMap == nil {
				t.Fatal("beads-config should be ConfigMap")
			}
			if v.VolumeSource.ConfigMap.Name != "agent-config" {
				t.Errorf("ConfigMap name = %q, want agent-config", v.VolumeSource.ConfigMap.Name)
			}
			return
		}
	}
	t.Error("beads-config volume not found")
}

func TestBuildVolumes_CredentialsSecret(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.CredentialsSecret = "claude-creds"
	volumes := m.buildVolumes(spec)

	for _, v := range volumes {
		if v.Name == VolumeClaudeCreds {
			if v.VolumeSource.Secret == nil {
				t.Fatal("claude-creds should be Secret volume")
			}
			if v.VolumeSource.Secret.SecretName != "claude-creds" {
				t.Errorf("Secret name = %q, want claude-creds", v.VolumeSource.Secret.SecretName)
			}
			return
		}
	}
	t.Error("claude-creds volume not found")
}

// TestBuildVolumes_GitCreds verifies the W6 (ka-0000002787) projected
// volume that materializes git credentials as files in the init-clone
// container instead of env vars.
func TestBuildVolumes_GitCreds(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.GitCredentialsSecret = "git-creds"
	spec.GitlabTokenSecret = "gitlab-token"
	volumes := m.buildVolumes(spec)

	var v *corev1.Volume
	for i := range volumes {
		if volumes[i].Name == VolumeGitCreds {
			v = &volumes[i]
			break
		}
	}
	if v == nil {
		t.Fatal("git-creds projected volume not found")
	}
	if v.VolumeSource.Projected == nil {
		t.Fatal("git-creds must be a Projected volume")
	}
	if v.VolumeSource.Projected.DefaultMode == nil || *v.VolumeSource.Projected.DefaultMode != GitCredFileMode {
		t.Errorf("git-creds DefaultMode = %v, want %d (0400)", v.VolumeSource.Projected.DefaultMode, GitCredFileMode)
	}
	if len(v.VolumeSource.Projected.Sources) != 2 {
		t.Fatalf("expected 2 projection sources (github + gitlab), got %d", len(v.VolumeSource.Projected.Sources))
	}

	// First source: GitCredentialsSecret projecting username + token under github/.
	gh := v.VolumeSource.Projected.Sources[0].Secret
	if gh == nil || gh.Name != "git-creds" {
		t.Fatalf("first projection must reference git-creds Secret, got: %+v", v.VolumeSource.Projected.Sources[0])
	}
	wantPaths := map[string]string{"username": "github/username", "token": "github/token"}
	for _, item := range gh.Items {
		if want, ok := wantPaths[item.Key]; !ok {
			t.Errorf("unexpected key %q in github projection", item.Key)
		} else if item.Path != want {
			t.Errorf("github key %q path = %q, want %q", item.Key, item.Path, want)
		}
	}

	// Second source: GitlabTokenSecret projecting token under gitlab/.
	gl := v.VolumeSource.Projected.Sources[1].Secret
	if gl == nil || gl.Name != "gitlab-token" {
		t.Fatalf("second projection must reference gitlab-token Secret, got: %+v", v.VolumeSource.Projected.Sources[1])
	}
	if len(gl.Items) != 1 || gl.Items[0].Key != "token" || gl.Items[0].Path != "gitlab/token" {
		t.Errorf("gitlab projection items = %+v, want token -> gitlab/token", gl.Items)
	}
}

// TestBuildVolumes_GitCredsGithubOnly verifies the projected volume is
// emitted with only the github source when GitlabTokenSecret is unset.
func TestBuildVolumes_GitCredsGithubOnly(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.GitCredentialsSecret = "git-creds"
	volumes := m.buildVolumes(spec)

	var v *corev1.Volume
	for i := range volumes {
		if volumes[i].Name == VolumeGitCreds {
			v = &volumes[i]
			break
		}
	}
	if v == nil {
		t.Fatal("git-creds projected volume not found")
	}
	if v.VolumeSource.Projected == nil {
		t.Fatal("git-creds must be a Projected volume")
	}
	if len(v.VolumeSource.Projected.Sources) != 1 {
		t.Fatalf("expected 1 projection source (github only), got %d", len(v.VolumeSource.Projected.Sources))
	}
}

// TestBuildVolumes_GitCredsNone verifies that no git-creds volume is
// emitted when neither secret is configured.
func TestBuildVolumes_GitCredsNone(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	volumes := m.buildVolumes(spec)
	for _, v := range volumes {
		if v.Name == VolumeGitCreds {
			t.Error("git-creds volume must not be present when no git secrets configured")
		}
	}
}

func TestBuildVolumes_SshKeys(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.SshKeys = map[string]SshKeySource{
		"perf-bastion": {SecretName: "perf-bastion-key", SecretKey: "id_ed25519"},
		"deploy":       {SecretName: "deploy-key", SecretKey: "private"},
	}
	volumes := m.buildVolumes(spec)

	volMap := make(map[string]corev1.Volume)
	for _, v := range volumes {
		volMap[v.Name] = v
	}

	for name, src := range spec.SshKeys {
		volName := VolumeSshKeyPrefix + name
		v, ok := volMap[volName]
		if !ok {
			t.Fatalf("missing ssh key volume %q", volName)
		}
		if v.VolumeSource.Secret == nil {
			t.Fatalf("%s: expected Secret volume source", volName)
		}
		if v.VolumeSource.Secret.SecretName != src.SecretName {
			t.Errorf("%s: SecretName = %q, want %q", volName,
				v.VolumeSource.Secret.SecretName, src.SecretName)
		}
		if v.VolumeSource.Secret.DefaultMode == nil ||
			*v.VolumeSource.Secret.DefaultMode != SshKeyFileMode {
			t.Errorf("%s: DefaultMode = %v, want %#o", volName,
				v.VolumeSource.Secret.DefaultMode, SshKeyFileMode)
		}
		items := v.VolumeSource.Secret.Items
		if len(items) != 1 {
			t.Fatalf("%s: expected 1 projected item, got %d", volName, len(items))
		}
		if items[0].Key != src.SecretKey {
			t.Errorf("%s: item Key = %q, want %q", volName, items[0].Key, src.SecretKey)
		}
		if items[0].Path != name {
			t.Errorf("%s: item Path = %q, want %q", volName, items[0].Path, name)
		}
		if items[0].Mode == nil || *items[0].Mode != SshKeyFileMode {
			t.Errorf("%s: item Mode = %v, want %#o", volName, items[0].Mode, SshKeyFileMode)
		}
	}
}

func TestBuildVolumes_SshKeysDeterministicOrder(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.SshKeys = map[string]SshKeySource{
		"zeta":  {SecretName: "z", SecretKey: "k"},
		"alpha": {SecretName: "a", SecretKey: "k"},
		"mike":  {SecretName: "m", SecretKey: "k"},
	}
	want := []string{
		VolumeSshKeyPrefix + "alpha",
		VolumeSshKeyPrefix + "mike",
		VolumeSshKeyPrefix + "zeta",
	}
	for i := range 5 {
		volumes := m.buildVolumes(spec)
		var got []string
		for _, v := range volumes {
			if strings.HasPrefix(v.Name, VolumeSshKeyPrefix) {
				got = append(got, v.Name)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("got %d ssh volumes, want %d: %v", len(got), len(want), got)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("iter %d: ssh volume[%d] = %q, want %q", i, j, got[j], want[j])
			}
		}
	}
}

func TestBuildVolumes_SshKeysEmpty(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	volumes := m.buildVolumes(spec)
	for _, v := range volumes {
		if strings.HasPrefix(v.Name, VolumeSshKeyPrefix) {
			t.Errorf("unexpected ssh key volume without SshKeys: %s", v.Name)
		}
	}
}

// W5 (ka-0000006585): NATS token is projected as a tmpfs Secret volume,
// mode 0400, single file at /etc/nats-token/token.
func TestBuildVolumes_NatsToken(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.NatsTokenSecret = "gardenio-nats-agent-creds"
	volumes := m.buildVolumes(spec)

	for _, v := range volumes {
		if v.Name != VolumeNatsToken {
			continue
		}
		if v.VolumeSource.Secret == nil {
			t.Fatal("nats-token volume must use Secret source")
		}
		if v.VolumeSource.Secret.SecretName != "gardenio-nats-agent-creds" {
			t.Errorf("SecretName = %q, want %q",
				v.VolumeSource.Secret.SecretName, "gardenio-nats-agent-creds")
		}
		if v.VolumeSource.Secret.DefaultMode == nil ||
			*v.VolumeSource.Secret.DefaultMode != NatsTokenFileMode {
			t.Errorf("DefaultMode = %v, want %#o",
				v.VolumeSource.Secret.DefaultMode, NatsTokenFileMode)
		}
		items := v.VolumeSource.Secret.Items
		if len(items) != 1 {
			t.Fatalf("expected 1 projected item, got %d", len(items))
		}
		if items[0].Key != "token" || items[0].Path != NatsTokenFileName {
			t.Errorf("item = %+v, want key=token path=%s", items[0], NatsTokenFileName)
		}
		return
	}
	t.Error("nats-token volume not found")
}

func TestBuildVolumes_NatsTokenAbsent(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec() // no NatsTokenSecret
	volumes := m.buildVolumes(spec)
	for _, v := range volumes {
		if v.Name == VolumeNatsToken {
			t.Errorf("nats-token volume must not exist without NatsTokenSecret")
		}
	}
}

// gd-W8OwRoHLLn: object-store credentials are projected as a tmpfs Secret
// volume, mode 0400, two files (access_key_id, secret_access_key) — mirrors
// the NATS token pattern above.
func TestBuildVolumes_ObjectStoreCreds(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.ObjectStoreCredentialsSecret = "gardenio-minio-agent-credentials"
	volumes := m.buildVolumes(spec)

	for _, v := range volumes {
		if v.Name != VolumeObjectStoreCreds {
			continue
		}
		if v.VolumeSource.Secret == nil {
			t.Fatal("object-store-creds volume must use Secret source")
		}
		if v.VolumeSource.Secret.SecretName != "gardenio-minio-agent-credentials" {
			t.Errorf("SecretName = %q, want %q",
				v.VolumeSource.Secret.SecretName, "gardenio-minio-agent-credentials")
		}
		if v.VolumeSource.Secret.DefaultMode == nil ||
			*v.VolumeSource.Secret.DefaultMode != ObjectStoreCredFileMode {
			t.Errorf("DefaultMode = %v, want %#o",
				v.VolumeSource.Secret.DefaultMode, ObjectStoreCredFileMode)
		}
		items := v.VolumeSource.Secret.Items
		if len(items) != 2 {
			t.Fatalf("expected 2 projected items, got %d", len(items))
		}
		if items[0].Key != "accesskey" || items[0].Path != ObjectStoreAccessKeyFileName {
			t.Errorf("item[0] = %+v, want key=accesskey path=%s", items[0], ObjectStoreAccessKeyFileName)
		}
		if items[1].Key != "secretkey" || items[1].Path != ObjectStoreSecretKeyFileName {
			t.Errorf("item[1] = %+v, want key=secretkey path=%s", items[1], ObjectStoreSecretKeyFileName)
		}
		return
	}
	t.Error("object-store-creds volume not found")
}

// AWS/IRSA regression: without ObjectStoreCredentialsSecret, no such volume
// is created — the pod spec must be unaffected outside endpoint mode.
func TestBuildVolumes_ObjectStoreCredsAbsent(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec() // no ObjectStoreCredentialsSecret
	volumes := m.buildVolumes(spec)
	for _, v := range volumes {
		if v.Name == VolumeObjectStoreCreds {
			t.Errorf("object-store-creds volume must not exist without ObjectStoreCredentialsSecret")
		}
	}
}

// --- buildVolumeMounts tests ---

func TestBuildVolumeMounts_Minimal(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	mounts := m.buildVolumeMounts(spec)

	// Build map by MountPath since multiple mounts can use the same volume.
	mountByPath := make(map[string]corev1.VolumeMount)
	for _, vm := range mounts {
		mountByPath[vm.MountPath] = vm
	}

	if ws, ok := mountByPath[MountWorkspace]; !ok {
		t.Error("missing workspace mount")
	} else if ws.Name != VolumeWorkspace {
		t.Errorf("workspace volume name = %q, want %q", ws.Name, VolumeWorkspace)
	}

	if tmp, ok := mountByPath[MountTmp]; !ok {
		t.Error("missing tmp mount")
	} else if tmp.Name != VolumeTmp {
		t.Errorf("tmp volume name = %q, want %q", tmp.Name, VolumeTmp)
	}

	// Claude state mount is always present.
	if cs, ok := mountByPath[MountClaudeState]; !ok {
		t.Error("missing claude state mount")
	} else if cs.SubPath != SubPathClaudeState {
		t.Errorf("claude state SubPath = %q, want %q", cs.SubPath, SubPathClaudeState)
	}
}

func TestBuildVolumeMounts_ClaudeState(t *testing.T) {
	m := newTestManager()

	// Claude state mount is always present (S3 handles cross-pod persistence).
	for _, tc := range []struct {
		name string
		spec AgentPodSpec
	}{
		{"with_pvc", func() AgentPodSpec {
			s := minimalSpec()
			s.WorkspaceStorage = &WorkspaceStorageSpec{Size: "5Gi"}
			return s
		}()},
		{"emptydir", minimalSpec()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mounts := m.buildVolumeMounts(tc.spec)
			found := false
			for _, vm := range mounts {
				if vm.MountPath == MountClaudeState {
					found = true
					if vm.SubPath != SubPathClaudeState {
						t.Errorf("SubPath = %q, want %q", vm.SubPath, SubPathClaudeState)
					}
				}
			}
			if !found {
				t.Errorf("Claude state mount not found for %s", tc.name)
			}
		})
	}
}

func TestBuildVolumeMounts_ConfigMapReadOnly(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.ConfigMapName = "agent-config"
	mounts := m.buildVolumeMounts(spec)

	for _, vm := range mounts {
		if vm.Name == VolumeBeadsConfig {
			if !vm.ReadOnly {
				t.Error("beads-config mount should be ReadOnly")
			}
			if vm.MountPath != MountBeadsConfig {
				t.Errorf("mount path = %q, want %q", vm.MountPath, MountBeadsConfig)
			}
			return
		}
	}
	t.Error("beads-config mount not found")
}

func TestBuildVolumeMounts_Credentials(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.CredentialsSecret = "claude-creds"
	mounts := m.buildVolumeMounts(spec)

	for _, vm := range mounts {
		if vm.Name == VolumeClaudeCreds {
			if !vm.ReadOnly {
				t.Error("credentials mount should be ReadOnly")
			}
			if vm.MountPath != MountClaudeCreds {
				t.Errorf("mount path = %q, want %q", vm.MountPath, MountClaudeCreds)
			}
			return
		}
	}
	t.Error("credentials mount not found")
}

func TestBuildVolumeMounts_SshKeys(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.SshKeys = map[string]SshKeySource{
		"perf-bastion": {SecretName: "perf-bastion-key", SecretKey: "id_ed25519"},
		"deploy":       {SecretName: "deploy-key", SecretKey: "private"},
	}
	mounts := m.buildVolumeMounts(spec)

	mountByPath := make(map[string]corev1.VolumeMount)
	for _, vm := range mounts {
		mountByPath[vm.MountPath] = vm
	}

	for name := range spec.SshKeys {
		path := MountSshKeys + "/" + name
		vm, ok := mountByPath[path]
		if !ok {
			t.Fatalf("missing ssh key mount at %s", path)
		}
		if vm.Name != VolumeSshKeyPrefix+name {
			t.Errorf("%s: Name = %q, want %q", path, vm.Name, VolumeSshKeyPrefix+name)
		}
		if vm.SubPath != name {
			t.Errorf("%s: SubPath = %q, want %q", path, vm.SubPath, name)
		}
		if !vm.ReadOnly {
			t.Errorf("%s: expected ReadOnly mount", path)
		}
	}
}

func TestBuildVolumeMounts_NatsToken(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.NatsTokenSecret = "gardenio-nats-agent-creds"
	mounts := m.buildVolumeMounts(spec)

	for _, vm := range mounts {
		if vm.Name == VolumeNatsToken {
			if vm.MountPath != MountNatsToken {
				t.Errorf("MountPath = %q, want %q", vm.MountPath, MountNatsToken)
			}
			if !vm.ReadOnly {
				t.Error("nats-token mount must be ReadOnly")
			}
			return
		}
	}
	t.Error("nats-token mount not found")
}

func TestBuildVolumeMounts_NatsTokenAbsent(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	mounts := m.buildVolumeMounts(spec)
	for _, vm := range mounts {
		if vm.Name == VolumeNatsToken {
			t.Errorf("nats-token mount must not exist without NatsTokenSecret")
		}
	}
}

func TestBuildVolumeMounts_ObjectStoreCreds(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.ObjectStoreCredentialsSecret = "gardenio-minio-agent-credentials"
	mounts := m.buildVolumeMounts(spec)

	for _, vm := range mounts {
		if vm.Name == VolumeObjectStoreCreds {
			if vm.MountPath != MountObjectStoreCreds {
				t.Errorf("MountPath = %q, want %q", vm.MountPath, MountObjectStoreCreds)
			}
			if !vm.ReadOnly {
				t.Error("object-store-creds mount must be ReadOnly")
			}
			return
		}
	}
	t.Error("object-store-creds mount not found")
}

func TestBuildVolumeMounts_ObjectStoreCredsAbsent(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	mounts := m.buildVolumeMounts(spec)
	for _, vm := range mounts {
		if vm.Name == VolumeObjectStoreCreds {
			t.Errorf("object-store-creds mount must not exist without ObjectStoreCredentialsSecret")
		}
	}
}

func TestBuildVolumeMounts_SshKeysEmpty(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	mounts := m.buildVolumeMounts(spec)
	for _, vm := range mounts {
		if strings.HasPrefix(vm.Name, VolumeSshKeyPrefix) {
			t.Errorf("unexpected ssh key mount without SshKeys: %s", vm.Name)
		}
	}
}

// --- initclone tests ---

func TestBuildInitCloneContainer_Nil(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	if ic := m.buildInitCloneContainer(spec); ic != nil {
		t.Error("expected nil init container without GitURL or ReferenceRepos")
	}
}

func TestBuildInitCloneContainer_WithGitURL(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.GitURL = "https://github.com/example/repo.git"
	ic := m.buildInitCloneContainer(spec)

	if ic == nil {
		t.Fatal("expected init container with GitURL")
	}
	if ic.Name != InitCloneName {
		t.Errorf("name = %q, want %q", ic.Name, InitCloneName)
	}
	if ic.Image != InitCloneImage {
		t.Errorf("image = %q, want %q", ic.Image, InitCloneImage)
	}

	// init-clone runs as the agent UID (PSA restricted) — see
	// TestBuildInitCloneContainer_RestrictedSecurityContext for the full check.
	if ic.SecurityContext == nil || ic.SecurityContext.RunAsUser == nil {
		t.Fatal("missing security context")
	}
	if *ic.SecurityContext.RunAsUser != AgentUID {
		t.Errorf("RunAsUser = %d, want %d (AgentUID)", *ic.SecurityContext.RunAsUser, AgentUID)
	}

	// Script should reference the git URL.
	script := ic.Command[2]
	if !strings.Contains(script, spec.GitURL) {
		t.Error("script should contain GitURL")
	}
	// init-clone image now ships git pre-installed; runtime apk install would
	// require root and break PSA restricted admission.
	if strings.Contains(script, "apk add") {
		t.Error("script must not run apk add at runtime (image has git pre-installed)")
	}
	// Should set default branch to main.
	if !strings.Contains(script, "main") {
		t.Error("script should use default branch 'main'")
	}
}

// TestBuildInitCloneContainer_GitCredentials pins W6 (ka-0000002787): the
// init container reads git creds from a projected file mount rather than
// GIT_USERNAME / GIT_TOKEN env vars, which leak via /proc/<pid>/environ,
// `ps eww`, and `kubectl describe pod`.
func TestBuildInitCloneContainer_GitCredentials(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.GitURL = "https://github.com/example/repo.git"
	spec.GitCredentialsSecret = "git-creds"

	ic := m.buildInitCloneContainer(spec)
	script := ic.Command[2]

	if !strings.Contains(script, "credential.helper") {
		t.Error("script should set up credential helper")
	}

	envMap := make(map[string]corev1.EnvVar)
	for _, ev := range ic.Env {
		envMap[ev.Name] = ev
	}
	if _, ok := envMap["GIT_USERNAME"]; ok {
		t.Error("init container must NOT receive GIT_USERNAME env var (W6)")
	}
	if _, ok := envMap["GIT_TOKEN"]; ok {
		t.Error("init container must NOT receive GIT_TOKEN env var (W6)")
	}

	// Init container must mount the projected git-creds volume read-only.
	var mnt *corev1.VolumeMount
	for i := range ic.VolumeMounts {
		if ic.VolumeMounts[i].Name == VolumeGitCreds {
			mnt = &ic.VolumeMounts[i]
			break
		}
	}
	if mnt == nil {
		t.Fatalf("init container missing git-creds volume mount, got: %+v", ic.VolumeMounts)
	}
	if mnt.MountPath != MountGitCreds {
		t.Errorf("git-creds mount path = %q, want %q", mnt.MountPath, MountGitCreds)
	}
	if !mnt.ReadOnly {
		t.Error("git-creds mount must be read-only")
	}
}

// TestBuildInitCloneContainer_GitlabToken pins W6: GitLab token consumed
// from /etc/git-creds/gitlab/token, never as GITLAB_TOKEN env var.
func TestBuildInitCloneContainer_GitlabToken(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.GitURL = "https://gitlab.com/example/repo.git"
	spec.GitlabTokenSecret = "gitlab-token"

	ic := m.buildInitCloneContainer(spec)
	script := ic.Command[2]

	if !strings.Contains(script, "/etc/git-creds/gitlab/token") {
		t.Error("script should read GitLab token from /etc/git-creds/gitlab/token")
	}

	envMap := make(map[string]corev1.EnvVar)
	for _, ev := range ic.Env {
		envMap[ev.Name] = ev
	}
	if _, ok := envMap["GITLAB_TOKEN"]; ok {
		t.Error("init container must NOT receive GITLAB_TOKEN env var (W6)")
	}
}

func TestBuildInitCloneContainer_Resources(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.GitURL = "https://github.com/example/repo.git"
	ic := m.buildInitCloneContainer(spec)

	cpuReq := ic.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "100m" {
		t.Errorf("init CPU request = %s, want 100m", cpuReq.String())
	}
	memLimit := ic.Resources.Limits[corev1.ResourceMemory]
	if memLimit.String() != "512Mi" {
		t.Errorf("init memory limit = %s, want 512Mi", memLimit.String())
	}
}

// TestBuildInitCloneContainer_RestrictedSecurityContext pins the security
// context shape required for admission under PSA "restricted" — see
// ka-0000002495. If any of these checks regress, agent pods in
// gardenio-e2e / gardenio-next will be rejected at admission time.
func TestBuildInitCloneContainer_RestrictedSecurityContext(t *testing.T) {
	mgr := New(fake.NewSimpleClientset(), testLogger())
	spec := AgentPodSpec{
		Mode: "crew", Project: "proj", Role: "dev", AgentName: "alpha",
		GitURL: "https://github.com/org/repo.git",
	}

	ic := mgr.buildInitCloneContainer(spec)
	sc := ic.SecurityContext
	if sc == nil {
		t.Fatal("init container missing SecurityContext")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != AgentUID {
		t.Errorf("RunAsUser = %v, want %d", sc.RunAsUser, AgentUID)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != AgentGID {
		t.Errorf("RunAsGroup = %v, want %d", sc.RunAsGroup, AgentGID)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("RunAsNonRoot must be true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("Capabilities.Drop must contain ALL, got %+v", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile must be RuntimeDefault, got %+v", sc.SeccompProfile)
	}
}

// TestBuildInitCloneContainer_ImageOverride verifies the InitCloneImageOverride
// (wired from cfg.InitCloneImage / env INIT_CLONE_IMAGE) replaces the default.
func TestBuildInitCloneContainer_ImageOverride(t *testing.T) {
	mgr := New(fake.NewSimpleClientset(), testLogger())
	mgr.InitCloneImageOverride = "registry.example.com/init-clone:v1.2.3"
	spec := AgentPodSpec{
		Mode: "crew", Project: "proj", Role: "dev", AgentName: "alpha",
		GitURL: "https://github.com/org/repo.git",
	}
	ic := mgr.buildInitCloneContainer(spec)
	if ic.Image != "registry.example.com/init-clone:v1.2.3" {
		t.Errorf("image = %q, want override value", ic.Image)
	}
}

func TestBuildInitCloneContainer_WorkspaceMount(t *testing.T) {
	mgr := New(fake.NewSimpleClientset(), testLogger())
	spec := AgentPodSpec{
		Mode: "crew", Project: "proj", Role: "dev", AgentName: "alpha",
		GitURL: "https://github.com/org/repo.git",
	}

	ic := mgr.buildInitCloneContainer(spec)
	// When no git credentials are configured, only the workspace volume is
	// mounted. With creds, the projected git-creds mount is also added (see
	// TestBuildInitCloneContainer_GitCredentials).
	if len(ic.VolumeMounts) != 1 {
		t.Fatalf("expected 1 volume mount, got %d", len(ic.VolumeMounts))
	}
	if ic.VolumeMounts[0].Name != VolumeWorkspace {
		t.Errorf("expected workspace volume mount, got %s", ic.VolumeMounts[0].Name)
	}
}

// TestBuildInitCloneContainer_NoChown verifies the script no longer runs
// `chown` — the container runs as AgentUID/AgentGID directly and the
// pod-level FSGroup recursively chgrp's the workspace volume, so the
// previous root-mode chown is unnecessary (and would fail under restricted
// PSA admission with CAP_CHOWN dropped).
func TestBuildInitCloneContainer_NoChown(t *testing.T) {
	mgr := New(fake.NewSimpleClientset(), testLogger())
	spec := AgentPodSpec{
		Mode: "crew", Project: "myproj", Role: "dev", AgentName: "alpha",
		GitURL: "https://github.com/org/repo.git",
	}

	ic := mgr.buildInitCloneContainer(spec)
	script := ic.Command[2]
	if strings.Contains(script, "chown") {
		t.Errorf("script must not invoke chown (runs as non-root, FSGroup handles ownership):\n%s", script)
	}
}

func TestBuildVolumeMounts_HarnessState(t *testing.T) {
	m := newTestManager()
	for _, tc := range []struct {
		harness, mount, subPath string
		absent                  string
	}{
		{"", MountClaudeState, SubPathClaudeState, MountCodexState},
		{"claude", MountClaudeState, SubPathClaudeState, MountCodexState},
		{"codex", MountCodexState, SubPathCodexState, MountClaudeState},
	} {
		t.Run("harness="+tc.harness, func(t *testing.T) {
			spec := minimalSpec()
			spec.Harness = tc.harness
			var found bool
			for _, vm := range m.buildVolumeMounts(spec) {
				if vm.MountPath == tc.absent {
					t.Errorf("unexpected mount at %s", tc.absent)
				}
				if vm.MountPath == tc.mount {
					found = true
					if vm.Name != VolumeWorkspace || vm.SubPath != tc.subPath {
						t.Errorf("mount = %s/%s, want %s/%s", vm.Name, vm.SubPath, VolumeWorkspace, tc.subPath)
					}
				}
			}
			if !found {
				t.Errorf("state mount %s not found", tc.mount)
			}
		})
	}
}

func TestBuildVolumes_CodexAuth(t *testing.T) {
	m := newTestManager()

	// Absent unless the controller set a Codex login Secret.
	spec := minimalSpec()
	for _, v := range m.buildVolumes(spec) {
		if v.Name == VolumeCodexAuth {
			t.Fatal("codex-auth volume must not be present without CodexAuthSecret")
		}
	}
	for _, vm := range m.buildVolumeMounts(spec) {
		if vm.Name == VolumeCodexAuth {
			t.Fatal("codex-auth mount must not be present without CodexAuthSecret")
		}
	}

	spec.CodexAuthSecret = "gardenio-codex-auth-session"
	var vol *corev1.Volume
	for _, v := range m.buildVolumes(spec) {
		if v.Name == VolumeCodexAuth {
			v := v
			vol = &v
		}
	}
	if vol == nil || vol.Secret == nil {
		t.Fatal("expected a codex-auth Secret volume")
	}
	if vol.Secret.SecretName != "gardenio-codex-auth-session" {
		t.Errorf("SecretName = %q", vol.Secret.SecretName)
	}
	if len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != CodexAuthFileName || vol.Secret.Items[0].Path != CodexAuthFileName {
		t.Errorf("items = %+v, want only auth.json", vol.Secret.Items)
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != CodexAuthFileMode {
		t.Errorf("DefaultMode = %v, want %#o", vol.Secret.DefaultMode, CodexAuthFileMode)
	}

	var found bool
	for _, vm := range m.buildVolumeMounts(spec) {
		if vm.Name != VolumeCodexAuth {
			continue
		}
		found = true
		if vm.MountPath != MountCodexAuth || !vm.ReadOnly {
			t.Errorf("mount = %+v, want read-only at %s", vm, MountCodexAuth)
		}
	}
	if !found {
		t.Error("codex-auth mount missing")
	}

	// The admission gate must wait for the -session Secret like any other.
	var named bool
	for _, n := range referencedSecretNames(spec) {
		if n == "gardenio-codex-auth-session" {
			named = true
		}
	}
	if !named {
		t.Error("referencedSecretNames must include CodexAuthSecret so verifySecretsExist gates on it")
	}
}

func TestBuildVolumes_GeminiAuth(t *testing.T) {
	m := newTestManager()

	spec := minimalSpec()
	for _, v := range m.buildVolumes(spec) {
		if v.Name == VolumeGeminiAuth {
			t.Fatal("gemini-auth volume must not be present without GeminiOAuthSecret")
		}
	}
	for _, vm := range m.buildVolumeMounts(spec) {
		if vm.Name == VolumeGeminiAuth {
			t.Fatal("gemini-auth mount must not be present without GeminiOAuthSecret")
		}
	}

	spec.GeminiOAuthSecret = "gardenio-gemini-login"
	var vol *corev1.Volume
	for _, v := range m.buildVolumes(spec) {
		if v.Name == VolumeGeminiAuth {
			v := v
			vol = &v
		}
	}
	if vol == nil || vol.Secret == nil || vol.Secret.SecretName != "gardenio-gemini-login" {
		t.Fatalf("expected a gemini-auth Secret volume for gardenio-gemini-login, got %+v", vol)
	}
	if len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != GeminiOAuthFileName {
		t.Errorf("items = %+v, want only oauth_creds.json", vol.Secret.Items)
	}
	var found bool
	for _, vm := range m.buildVolumeMounts(spec) {
		if vm.Name == VolumeGeminiAuth {
			found = true
			if vm.MountPath != MountGeminiAuth || !vm.ReadOnly {
				t.Errorf("mount = %+v, want read-only at %s", vm, MountGeminiAuth)
			}
		}
	}
	if !found {
		t.Error("gemini-auth mount missing")
	}
	var named bool
	for _, n := range referencedSecretNames(spec) {
		if n == "gardenio-gemini-login" {
			named = true
		}
	}
	if !named {
		t.Error("referencedSecretNames must include GeminiOAuthSecret so verifySecretsExist gates on it")
	}
}

func TestBuildVolumes_SlackViewSession(t *testing.T) {
	m := newTestManager()

	spec := minimalSpec()
	for _, v := range m.buildVolumes(spec) {
		if v.Name == VolumeSlackViewSession {
			t.Fatal("slack-view-session volume must not be present without SlackViewSessionSecret")
		}
	}
	for _, vm := range m.buildVolumeMounts(spec) {
		if vm.Name == VolumeSlackViewSession {
			t.Fatal("slack-view-session mount must not be present without SlackViewSessionSecret")
		}
	}

	spec.SlackViewSessionSecret = "slack-e2e-session"
	var vol *corev1.Volume
	for _, v := range m.buildVolumes(spec) {
		if v.Name == VolumeSlackViewSession {
			v := v
			vol = &v
		}
	}
	if vol == nil || vol.Secret == nil {
		t.Fatal("expected a slack-view-session Secret volume")
	}
	if vol.Secret.SecretName != "slack-e2e-session" {
		t.Errorf("SecretName = %q", vol.Secret.SecretName)
	}
	if vol.Secret.Optional == nil || !*vol.Secret.Optional {
		t.Error("volume must be Optional so an unprovisioned Secret never blocks pod creation")
	}
	if len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != SlackViewSessionFileName || vol.Secret.Items[0].Path != SlackViewSessionFileName {
		t.Errorf("items = %+v, want only storage-state.json", vol.Secret.Items)
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != SlackViewSessionFileMode {
		t.Errorf("DefaultMode = %v, want %#o", vol.Secret.DefaultMode, SlackViewSessionFileMode)
	}

	var found bool
	for _, vm := range m.buildVolumeMounts(spec) {
		if vm.Name != VolumeSlackViewSession {
			continue
		}
		found = true
		if vm.MountPath != MountSlackViewSession || !vm.ReadOnly {
			t.Errorf("mount = %+v, want read-only at %s", vm, MountSlackViewSession)
		}
	}
	if !found {
		t.Error("expected a slack-view-session volume mount")
	}

	// The Secret is optional, so the admission gate must not wait on it.
	for _, n := range referencedSecretNames(spec) {
		if n == "slack-e2e-session" {
			t.Error("referencedSecretNames must exclude the optional Slack session Secret")
		}
	}
}
