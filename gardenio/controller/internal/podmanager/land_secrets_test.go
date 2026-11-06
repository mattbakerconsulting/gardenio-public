package podmanager

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func landTestSecret(ns, name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func getLandSecret(t *testing.T, client *fake.Clientset, ns, name string) *corev1.Secret {
	t.Helper()
	s, err := client.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", ns, name, err)
	}
	return s
}

func TestCopyLandSecret_OnlyTheListedKeys(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(landTestSecret("gardenio", "coopmux-token", map[string]string{"token": "t1", "admin": "fleet"}))
	c := LandSecretCopy{SourceNamespace: "gardenio", Name: "coopmux-token", Keys: []string{"token", "token", "absent"}}

	copied, err := CopyLandSecret(ctx, client, "land-acme", c)
	if err != nil || !copied {
		t.Fatalf("CopyLandSecret = %v, %v; want true, nil", copied, err)
	}
	got := getLandSecret(t, client, "land-acme", "coopmux-token")
	if !reflect.DeepEqual(got.Data, map[string][]byte{"token": []byte("t1")}) {
		t.Errorf("copy data = %q, want the token key alone", got.Data)
	}
	if !isLandSecret(got, LandSecretCopyType) || got.Annotations[AnnotationCopiedFrom] != "gardenio/coopmux-token" ||
		got.Annotations[AnnotationCopiedKeys] != "absent,token" {
		t.Errorf("copy metadata labels=%v annotations=%v", got.Labels, got.Annotations)
	}

	// A rotation reaches the copy.
	src := getLandSecret(t, client, "gardenio", "coopmux-token")
	src.Data["token"] = []byte("t2")
	if _, err := client.CoreV1().Secrets("gardenio").Update(ctx, src, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyLandSecret(ctx, client, "land-acme", c); err != nil {
		t.Fatal(err)
	}
	if v := string(getLandSecret(t, client, "land-acme", "coopmux-token").Data["token"]); v != "t2" {
		t.Errorf("copy token after rotation = %q, want t2", v)
	}
}

func TestCopyLandSecret_MissingSourceWritesNothing(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	copied, err := CopyLandSecret(ctx, client, "land-acme", LandSecretCopy{SourceNamespace: "gardenio", Name: "nats-agent-user", Keys: []string{"token"}})
	if err != nil || copied {
		t.Fatalf("CopyLandSecret = %v, %v; want false, nil", copied, err)
	}
	if _, err := client.CoreV1().Secrets("land-acme").Get(ctx, "nats-agent-user", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("copy written for a missing source: %v", err)
	}
}

// A Secret of the copy's name the controller did not copy (a door's minted
// token, say) is never overwritten.
func TestCopyLandSecret_RefusesAnotherSecret(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		landTestSecret("gardenio", "agent-token-gd-x", map[string]string{"token": "fleet"}),
		landTestSecret("land-acme", "agent-token-gd-x", map[string]string{"token": "mine"}),
	)
	_, err := CopyLandSecret(ctx, client, "land-acme", LandSecretCopy{SourceNamespace: "gardenio", Name: "agent-token-gd-x", Keys: []string{"token"}})
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("err = %v, want ErrNotOurs", err)
	}
	if v := string(getLandSecret(t, client, "land-acme", "agent-token-gd-x").Data["token"]); v != "mine" {
		t.Errorf("land secret overwritten: %q", v)
	}
}

func TestSyncLandSecrets(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		landTestSecret("gardenio", "lands-acme", map[string]string{"token": "o1"}),
		landTestSecret("gardenio", "lands-acme-old", map[string]string{"token": "old"}),
		landTestSecret("gardenio", "lands-beta", map[string]string{"token": "b1"}),
		landTestSecret("land-acme", "agent-token-gd-x", map[string]string{"token": "mine"}),
	)
	own := LandSecretCopy{SourceNamespace: "gardenio", Name: "lands-acme", Keys: []string{"token"}}
	for _, c := range []LandSecretCopy{
		own,
		// The land's account used to name another Secret.
		{SourceNamespace: "gardenio", Name: "lands-acme-old", Keys: []string{"token"}},
	} {
		if _, err := CopyLandSecret(ctx, client, "land-acme", c); err != nil {
			t.Fatal(err)
		}
	}
	// A copy whose annotation names a source the land may not read: the
	// sweep goes by the controller's list, never by the annotation.
	forged := landTestSecret("land-acme", "lands-acme", nil)
	if err := client.CoreV1().Secrets("land-acme").Delete(ctx, "lands-acme", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	forged.Labels = map[string]string{LabelSecretType: LandSecretCopyType, LabelManagedBy: ManagedByController}
	forged.Annotations = map[string]string{AnnotationCopiedFrom: "gardenio/lands-beta", AnnotationCopiedKeys: "token"}
	if _, err := client.CoreV1().Secrets("land-acme").Create(ctx, forged, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := SyncLandSecrets(ctx, client, "land-acme", LandSecrets{Copies: []LandSecretCopy{own}}); err != nil {
		t.Fatalf("SyncLandSecrets: %v", err)
	}
	for _, name := range []string{"lands-acme", "lands-acme-old"} {
		if _, err := client.CoreV1().Secrets("land-acme").Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("copy %s off the land's list kept: %v", name, err)
		}
	}
	if v := string(getLandSecret(t, client, "land-acme", "agent-token-gd-x").Data["token"]); v != "mine" {
		t.Errorf("sync touched a Secret it did not copy: %q", v)
	}

	// The listed copy follows a rotation, and goes when its source does.
	if _, err := CopyLandSecret(ctx, client, "land-acme", own); err != nil {
		t.Fatal(err)
	}
	src := getLandSecret(t, client, "gardenio", "lands-acme")
	src.Data["token"] = []byte("o2")
	if _, err := client.CoreV1().Secrets("gardenio").Update(ctx, src, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := SyncLandSecrets(ctx, client, "land-acme", LandSecrets{Copies: []LandSecretCopy{own}}); err != nil {
		t.Fatal(err)
	}
	if v := string(getLandSecret(t, client, "land-acme", "lands-acme").Data["token"]); v != "o2" {
		t.Errorf("rotated copy = %q, want o2", v)
	}
	if err := client.CoreV1().Secrets("gardenio").Delete(ctx, "lands-acme", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := SyncLandSecrets(ctx, client, "land-acme", LandSecrets{Copies: []LandSecretCopy{own}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("land-acme").Get(ctx, "lands-acme", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("copy of a deleted source kept: %v", err)
	}
}

func TestSyncLandSecrets_CoopmuxToken(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	if err := WriteLandCoopmuxToken(ctx, client, "land-acme", "acme", "tok-1"); err != nil {
		t.Fatal(err)
	}
	if err := SyncLandSecrets(ctx, client, "land-acme", LandSecrets{CoopmuxToken: "tok-2"}); err != nil {
		t.Fatal(err)
	}
	got := getLandSecret(t, client, "land-acme", LandCoopmuxTokenSecret)
	if v := string(got.Data["token"]); v != "tok-2" || !isLandSecret(got, LandCoopmuxTokenType) {
		t.Errorf("token after key rotation = %q labels %v, want the new token", v, got.Labels)
	}
	// No key any more: the token goes.
	if err := SyncLandSecrets(ctx, client, "land-acme", LandSecrets{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("land-acme").Get(ctx, LandCoopmuxTokenSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("land token kept with no key: %v", err)
	}
	// Nothing is created by a sweep.
	if err := SyncLandSecrets(ctx, client, "land-acme", LandSecrets{CoopmuxToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("land-acme").Get(ctx, LandCoopmuxTokenSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("sweep created a land token: %v", err)
	}
}

// The NATS password is synced as the coopmux token is, on its own: a sweep
// with the coopmux token unknown still rotates it, and one with no NATS
// password deletes it alone (gd-sS7bSXOdGs).
func TestSyncLandSecrets_NatsToken(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	if err := WriteLandCoopmuxToken(ctx, client, "land-acme", "acme", "mux-1"); err != nil {
		t.Fatal(err)
	}
	if err := WriteLandNatsToken(ctx, client, "land-acme", "acme", "nats-1"); err != nil {
		t.Fatal(err)
	}
	if err := SyncLandSecrets(ctx, client, "land-acme", LandSecrets{CoopmuxTokenUnknown: true, NatsToken: "nats-2"}); err != nil {
		t.Fatal(err)
	}
	got := getLandSecret(t, client, "land-acme", LandNatsTokenSecret)
	if v := string(got.Data["token"]); v != "nats-2" || !isLandSecret(got, LandNatsTokenType) {
		t.Errorf("nats password after rotation = %q labels %v, want the new one", v, got.Labels)
	}
	if v := string(getLandSecret(t, client, "land-acme", LandCoopmuxTokenSecret).Data["token"]); v != "mux-1" {
		t.Errorf("unknown coopmux token changed to %q", v)
	}
	if err := SyncLandSecrets(ctx, client, "land-acme", LandSecrets{CoopmuxToken: "mux-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("land-acme").Get(ctx, LandNatsTokenSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("nats password kept with land NATS off: %v", err)
	}
	getLandSecret(t, client, "land-acme", LandCoopmuxTokenSecret)
	// A coopmux token is not a NATS password: the types do not cross.
	if err := WriteLandNatsToken(ctx, fake.NewSimpleClientset(landTestSecret("land-acme", LandNatsTokenSecret, map[string]string{"token": "mine"})), "land-acme", "acme", "t"); !errors.Is(err, ErrNotOurs) {
		t.Errorf("WriteLandNatsToken over a foreign Secret: err = %v, want ErrNotOurs", err)
	}
}

// A spec that reads its land's NATS password gets it minted before the
// admission gate, and is refused when the manager cannot mint.
func TestCreateAgentPod_MintsLandNatsToken(t *testing.T) {
	ctx := context.Background()
	spec := AgentPodSpec{
		Project: "acme", Mode: "crew", Role: "crew", AgentName: "a1", Image: "agent:latest", Namespace: "land-acme",
		NatsTokenSecret: LandNatsTokenSecret,
		LandFenced:      true,
		LandNatsToken:   true,
	}
	client := fake.NewSimpleClientset()
	if err := New(client, testLogger()).CreateAgentPod(ctx, spec); err == nil {
		t.Fatal("CreateAgentPod with no NATS minter succeeded")
	}
	m := New(client, testLogger())
	m.MintLandNatsToken = func(land string) (string, error) { return "nats-land.v1." + land + ".mac", nil }
	if err := m.CreateAgentPod(ctx, spec); err != nil {
		t.Fatalf("CreateAgentPod: %v", err)
	}
	if v := string(getLandSecret(t, client, "land-acme", LandNatsTokenSecret).Data["token"]); v != "nats-land.v1.acme.mac" {
		t.Errorf("land NATS password = %q, want acme's", v)
	}
}

// A Secret of the token's name the controller did not mint, or a copy of
// that name, is never overwritten as a token.
func TestWriteLandCoopmuxToken_RefusesAnotherSecret(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(landTestSecret("land-acme", LandCoopmuxTokenSecret, map[string]string{"token": "mine"}))
	if err := WriteLandCoopmuxToken(ctx, client, "land-acme", "acme", "t"); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("err = %v, want ErrNotOurs", err)
	}
	if v := string(getLandSecret(t, client, "land-acme", LandCoopmuxTokenSecret).Data["token"]); v != "mine" {
		t.Errorf("Secret overwritten: %q", v)
	}
}

// A spec that reads its land's coopmux token gets it minted for its land
// before the admission gate, and is refused when the manager cannot mint.
func TestCreateAgentPod_MintsLandCoopmuxToken(t *testing.T) {
	ctx := context.Background()
	spec := AgentPodSpec{
		Project: "acme", Mode: "crew", Role: "crew", AgentName: "a1", Image: "agent:latest", Namespace: "land-acme",
		SecretEnv:        []SecretEnvSource{{EnvName: "COOP_MUX_TOKEN", SecretName: LandCoopmuxTokenSecret, SecretKey: "token"}},
		LandFenced:       true,
		LandCoopmuxToken: true,
	}
	client := fake.NewSimpleClientset()
	if err := New(client, testLogger()).CreateAgentPod(ctx, spec); err == nil {
		t.Fatal("CreateAgentPod with no minter succeeded")
	}

	m := New(client, testLogger())
	m.MintLandCoopmuxToken = func(land string) (string, error) { return "coopmux-land.v1." + land + ".mac", nil }
	if err := m.CreateAgentPod(ctx, spec); err != nil {
		t.Fatalf("CreateAgentPod: %v", err)
	}
	if v := string(getLandSecret(t, client, "land-acme", LandCoopmuxTokenSecret).Data["token"]); v != "coopmux-land.v1.acme.mac" {
		t.Errorf("land token = %q, want acme's", v)
	}
}

// CreateAgentPod copies only for a LandFenced spec; an unfenced spec's
// LandSecretCopies are ignored.
func TestCreateAgentPod_CopiesOnlyForFencedSpec(t *testing.T) {
	ctx := context.Background()
	newClient := func() *fake.Clientset {
		return fake.NewSimpleClientset(landTestSecret("gardenio", "coopmux-token", map[string]string{"token": "t1"}))
	}
	spec := AgentPodSpec{
		Project: "acme", Mode: "crew", Role: "crew", AgentName: "a1", Image: "agent:latest", Namespace: "land-acme",
		SecretEnv:        []SecretEnvSource{{EnvName: "COOP_BROKER_TOKEN", SecretName: "coopmux-token", SecretKey: "token"}},
		LandSecretCopies: []LandSecretCopy{{SourceNamespace: "gardenio", Name: "coopmux-token", Keys: []string{"token"}}},
	}

	client := newClient()
	err := New(client, testLogger()).CreateAgentPod(ctx, spec)
	var missing *MissingSecretsError
	if !errors.As(err, &missing) {
		t.Fatalf("unfenced spec: err = %v, want MissingSecretsError", err)
	}

	spec.LandFenced = true
	client = newClient()
	if err := New(client, testLogger()).CreateAgentPod(ctx, spec); err != nil {
		t.Fatalf("fenced spec: %v", err)
	}
	getLandSecret(t, client, "land-acme", "coopmux-token")
}

// A half the controller could not tell is left as it is: unknown copies are
// neither refreshed nor deleted, and an unknown token is not deleted.
func TestSyncLandSecrets_UnknownHalvesKept(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(landTestSecret("gardenio", "lands-acme", map[string]string{"token": "o1"}))
	if _, err := CopyLandSecret(ctx, client, "land-acme", LandSecretCopy{SourceNamespace: "gardenio", Name: "lands-acme", Keys: []string{"token"}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteLandCoopmuxToken(ctx, client, "land-acme", "acme", "tok-1"); err != nil {
		t.Fatal(err)
	}
	if err := SyncLandSecrets(ctx, client, "land-acme", LandSecrets{CopiesUnknown: true, CoopmuxTokenUnknown: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lands-acme", LandCoopmuxTokenSecret} {
		getLandSecret(t, client, "land-acme", name)
	}
	// Each half is decided on its own: a known empty copy list still deletes.
	if err := SyncLandSecrets(ctx, client, "land-acme", LandSecrets{CoopmuxTokenUnknown: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("land-acme").Get(ctx, "lands-acme", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("copy kept with a known empty list: %v", err)
	}
	getLandSecret(t, client, "land-acme", LandCoopmuxTokenSecret)
}
