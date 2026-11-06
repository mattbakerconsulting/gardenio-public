package podmanager

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// A tenant land's pod runs in land-<land> (Lands Track B §5.2) and reads
// Secrets the doors do not mint there (gd-gokoYMl5px, gd-ApgYe740ps): its
// land's coopmux token and NATS password, which the controller mints into the
// land namespace (LandCoopmuxTokenSecret, LandNatsTokenSecret,
// gd-sS7bSXOdGs), and its land's own Claude credential, the
// release-namespace Secret lands-<land>, of which only the read key is copied
// in (LandSecretCopy). Both are written at every pod create, so a new pod
// reads the current value, and on a sweep (SyncLandSecrets), which brings
// them up to date and deletes anything the land may no longer hold. They go
// with the namespace when gb lands close deletes it.

const (
	// LabelSecretType marks what kind of Secret the controller made.
	LabelSecretType = "gardenio.io/type"
	// LandSecretCopyType is LabelSecretType's value on a land copy,
	// LandCoopmuxTokenType on a land's minted coopmux token, and
	// LandNatsTokenType on its minted NATS password.
	LandSecretCopyType   = "land-secret-copy"
	LandCoopmuxTokenType = "land-coopmux-token"
	LandNatsTokenType    = "land-nats-token"
	// LabelManagedBy and ManagedByController mark what the controller made
	// (the same pair internal/lands/landns stamps on a land's objects).
	LabelManagedBy      = "app.kubernetes.io/managed-by"
	ManagedByController = "gardenio-controller"
	// AnnotationCopiedFrom is the source namespace/name of a land copy, and
	// AnnotationCopiedKeys the comma-separated keys it carries.
	AnnotationCopiedFrom = "gardenio.io/copied-from"
	AnnotationCopiedKeys = "gardenio.io/copied-keys"

	// LandCoopmuxTokenSecret is the Secret in a land namespace holding the
	// land's coopmux token under key "token".
	LandCoopmuxTokenSecret = "coopmux-land-token"
	// LandNatsTokenSecret is the Secret in a land namespace holding the
	// land's NATS password under key "token", projected like the fleet's
	// NATS token (NatsTokenSecret).
	LandNatsTokenSecret = "nats-land-token"
)

// LandTokenMinter returns a land's token. The controller sets one on the
// manager per token (landtoken.Mint and MintNATS under
// COOP_MUX_LAND_TOKEN_KEY_FILE).
type LandTokenMinter func(land string) (string, error)

// LandCoopmuxTokenMinter mints a land's coopmux token.
type LandCoopmuxTokenMinter = LandTokenMinter

// LandSecretCopy is one release-namespace Secret a tenant pod references:
// Name in SourceNamespace, of which only Keys are copied.
type LandSecretCopy struct {
	SourceNamespace string
	Name            string
	Keys            []string
}

// ErrNotOurs is returned when the land namespace already holds a Secret of
// the name the controller would write that the controller did not write
// there, or wrote as another kind. It is never overwritten.
var ErrNotOurs = errors.New("secret exists and is not the controller's land secret of that kind")

// LandSecrets is what one land namespace may hold besides the doors' Secrets:
// Copies, the release Secrets its pods read, CoopmuxToken, its coopmux token,
// and NatsToken, its NATS password ("" for none). The controller computes it
// from its own config and the land's account, never from what the namespace
// holds. CopiesUnknown, CoopmuxTokenUnknown and NatsTokenUnknown say the
// controller could not tell that part (no account cache entry for the land,
// a mint that failed): SyncLandSecrets then leaves it as it is, so a cache
// miss never deletes what running pods read.
type LandSecrets struct {
	Copies              []LandSecretCopy
	CopiesUnknown       bool
	CoopmuxToken        string
	CoopmuxTokenUnknown bool
	NatsToken           string
	NatsTokenUnknown    bool
}

// LandSecretSourceNamespace is the namespace the Secret name a spec references
// is read from: its copy's source for a LandFenced spec, spec.Namespace
// otherwise. A guard that probes a Secret before CreateAgentPod copies it
// probes here.
func LandSecretSourceNamespace(spec AgentPodSpec, name string) string {
	if spec.LandFenced {
		for _, c := range spec.LandSecretCopies {
			if c.Name == name && c.SourceNamespace != "" {
				return c.SourceNamespace
			}
		}
	}
	return spec.Namespace
}

// writeLandSecrets writes a LandFenced spec's land Secrets into
// spec.Namespace: its coopmux token and NATS password, when it reads them, and its
// LandSecretCopies. A copy source that does not exist is skipped, so
// verifySecretsExist reports it as missing and the reconciler retries.
func (m *K8sManager) writeLandSecrets(ctx context.Context, spec AgentPodSpec) error {
	if !spec.LandFenced {
		return nil
	}
	if spec.LandCoopmuxToken {
		if m.MintLandCoopmuxToken == nil {
			return fmt.Errorf("%s reads its land's coopmux token, and this controller cannot mint one", spec.AgentName)
		}
		token, err := m.MintLandCoopmuxToken(spec.Project)
		if err != nil {
			return fmt.Errorf("minting land %s's coopmux token: %w", spec.Project, err)
		}
		if err := WriteLandCoopmuxToken(ctx, m.client, spec.Namespace, spec.Project, token); err != nil {
			return err
		}
	}
	if spec.LandNatsToken {
		if m.MintLandNatsToken == nil {
			return fmt.Errorf("%s reads its land's NATS password, and this controller cannot mint one", spec.AgentName)
		}
		token, err := m.MintLandNatsToken(spec.Project)
		if err != nil {
			return fmt.Errorf("minting land %s's NATS password: %w", spec.Project, err)
		}
		if err := WriteLandNatsToken(ctx, m.client, spec.Namespace, spec.Project, token); err != nil {
			return err
		}
	}
	for _, c := range spec.LandSecretCopies {
		if c.SourceNamespace == "" || c.SourceNamespace == spec.Namespace {
			continue
		}
		copied, err := CopyLandSecret(ctx, m.client, spec.Namespace, c)
		if err != nil {
			return fmt.Errorf("copying secret %s/%s into %s: %w", c.SourceNamespace, c.Name, spec.Namespace, err)
		}
		if !copied {
			m.logger.Info("land secret source not found; the admission gate defers the pod",
				"secret", c.Name, "source", c.SourceNamespace, "namespace", spec.Namespace)
		}
	}
	return nil
}

// WriteLandCoopmuxToken writes land's coopmux token to LandCoopmuxTokenSecret
// in namespace, creating it or bringing it up to date. A Secret of that name
// the controller did not mint is refused with ErrNotOurs.
func WriteLandCoopmuxToken(ctx context.Context, client kubernetes.Interface, namespace, land, token string) error {
	return writeLandToken(ctx, client, namespace, LandCoopmuxTokenSecret, LandCoopmuxTokenType, token, "land "+land+"'s coopmux token")
}

// WriteLandNatsToken writes land's NATS password to LandNatsTokenSecret in
// namespace, as WriteLandCoopmuxToken does the coopmux token.
func WriteLandNatsToken(ctx context.Context, client kubernetes.Interface, namespace, land, token string) error {
	return writeLandToken(ctx, client, namespace, LandNatsTokenSecret, LandNatsTokenType, token, "land "+land+"'s NATS password")
}

func writeLandToken(ctx context.Context, client kubernetes.Interface, namespace, name, typ, token, what string) error {
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{LabelSecretType: typ, LabelManagedBy: ManagedByController},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"token": []byte(token)},
	}
	if err := writeLandSecret(ctx, client, want); err != nil {
		return fmt.Errorf("writing %s: %w", what, err)
	}
	return nil
}

// CopyLandSecret writes c's Keys from its source Secret to a Secret of the
// same name in namespace, creating it or bringing an earlier copy up to date.
// It reports false, and writes nothing, when the source does not exist. A
// key the source lacks is left out of the copy. A Secret of that name that
// is not a land copy is refused with ErrNotOurs.
func CopyLandSecret(ctx context.Context, client kubernetes.Interface, namespace string, c LandSecretCopy) (bool, error) {
	src, err := client.CoreV1().Secrets(c.SourceNamespace).Get(ctx, c.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading source: %w", err)
	}
	keys := slices.Sorted(slices.Values(c.Keys))
	keys = slices.Compact(keys)
	data := make(map[string][]byte, len(keys))
	for _, k := range keys {
		if v, ok := src.Data[k]; ok {
			data[k] = v
		}
	}
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.Name,
			Namespace: namespace,
			Labels:    map[string]string{LabelSecretType: LandSecretCopyType, LabelManagedBy: ManagedByController},
			Annotations: map[string]string{
				AnnotationCopiedFrom: c.SourceNamespace + "/" + c.Name,
				AnnotationCopiedKeys: strings.Join(keys, ","),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}

	if err := writeLandSecret(ctx, client, want); err != nil {
		return false, err
	}
	return true, nil
}

// writeLandSecret creates want, or brings the Secret of its name up to date
// with its data and annotations. One that is not the controller's of want's
// LabelSecretType is refused with ErrNotOurs.
func writeLandSecret(ctx context.Context, client kubernetes.Interface, want *corev1.Secret) error {
	secrets := client.CoreV1().Secrets(want.Namespace)
	cur, err := secrets.Get(ctx, want.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := secrets.Create(ctx, want, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating %s: %w", want.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", want.Name, err)
	}
	if !isLandSecret(cur, want.Labels[LabelSecretType]) {
		return ErrNotOurs
	}
	same := maps.EqualFunc(cur.Data, want.Data, func(a, b []byte) bool { return string(a) == string(b) })
	for k, v := range want.Annotations {
		same = same && cur.Annotations[k] == v
	}
	if same {
		return nil
	}
	cur.Data = want.Data
	cur.Annotations = mergeStrings(cur.Annotations, want.Annotations)
	if _, err := secrets.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating %s: %w", want.Name, err)
	}
	return nil
}

// SyncLandSecrets brings namespace's land Secrets in line with want. A copy
// want lists is brought up to date from its source and deleted when the source
// is gone, so a revoked credential does not outlive its source in a land. A
// copy want does not list is deleted, whatever its copied-from annotation
// says: a land whose account moved to another Secret, or a copy of anything
// else, keeps nothing. The coopmux token and NATS password are re-minted to
// want's values, and deleted when want has none. Nothing is created: a pod
// create writes what it reads. A part want marks unknown is left alone. It
// returns the errors joined.
func SyncLandSecrets(ctx context.Context, client kubernetes.Interface, namespace string, want LandSecrets) error {
	secrets := client.CoreV1().Secrets(namespace)
	var errs []error
	del := func(name, why string) {
		if err := secrets.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deleting land secret %s/%s (%s): %w", namespace, name, why, err))
		}
	}
	if !want.CopiesUnknown {
		errs = append(errs, syncLandCopies(ctx, client, namespace, want.Copies, del))
	}
	if !want.CoopmuxTokenUnknown {
		errs = append(errs, syncLandToken(ctx, client, namespace, LandCoopmuxTokenSecret, LandCoopmuxTokenType, want.CoopmuxToken, del))
	}
	if !want.NatsTokenUnknown {
		errs = append(errs, syncLandToken(ctx, client, namespace, LandNatsTokenSecret, LandNatsTokenType, want.NatsToken, del))
	}
	return errors.Join(errs...)
}

// syncLandCopies is SyncLandSecrets' copy half.
func syncLandCopies(ctx context.Context, client kubernetes.Interface, namespace string, copies []LandSecretCopy, del func(name, why string)) error {
	secrets := client.CoreV1().Secrets(namespace)
	var errs []error
	list, err := secrets.List(ctx, metav1.ListOptions{
		LabelSelector: LabelSecretType + "=" + LandSecretCopyType + "," + LabelManagedBy + "=" + ManagedByController,
	})
	if err != nil {
		return fmt.Errorf("listing land secret copies in %s: %w", namespace, err)
	}
	for i := range list.Items {
		cur := &list.Items[i]
		idx := slices.IndexFunc(copies, func(c LandSecretCopy) bool {
			return c.Name == cur.Name && cur.Annotations[AnnotationCopiedFrom] == c.SourceNamespace+"/"+c.Name
		})
		if idx < 0 {
			del(cur.Name, "not on the land's copy list")
			continue
		}
		copied, err := CopyLandSecret(ctx, client, namespace, copies[idx])
		if err != nil {
			errs = append(errs, fmt.Errorf("land secret copy %s/%s: %w", namespace, cur.Name, err))
			continue
		}
		if !copied {
			del(cur.Name, "source gone")
		}
	}
	return errors.Join(errs...)
}

// syncLandToken is SyncLandSecrets' part for the minted token Secret name of
// LabelSecretType typ.
func syncLandToken(ctx context.Context, client kubernetes.Interface, namespace, name, typ, token string, del func(name, why string)) error {
	cur, err := client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("reading %s/%s: %w", namespace, name, err)
	case !isLandSecret(cur, typ):
	case token == "":
		del(cur.Name, "no land token")
	default:
		if err := writeLandSecret(ctx, client, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: cur.Name, Namespace: namespace, Labels: cur.Labels},
			Data:       map[string][]byte{"token": []byte(token)},
		}); err != nil {
			return fmt.Errorf("%s/%s: %w", namespace, name, err)
		}
	}
	return nil
}

func isLandSecret(s *corev1.Secret, typ string) bool {
	return s.Labels[LabelSecretType] == typ && s.Labels[LabelManagedBy] == ManagedByController
}

func mergeStrings(have, want map[string]string) map[string]string {
	out := make(map[string]string, len(have)+len(want))
	maps.Copy(out, have)
	maps.Copy(out, want)
	return out
}
