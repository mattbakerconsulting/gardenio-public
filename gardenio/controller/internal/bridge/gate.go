package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// middlewareGVR is the GroupVersionResource for Traefik Middleware CRDs.
var middlewareGVR = schema.GroupVersionResource{
	Group:    "traefik.io",
	Version:  "v1alpha1",
	Resource: "middlewares",
}

// Bouncer manages IP whitelisting on Traefik Middleware CRDs via the K8s API.
type Bouncer struct {
	client    dynamic.Interface
	namespace string
	// middlewareNames are the Traefik Middleware CR names to manage.
	// e.g., ["gardenio-beads-ipwhitelist", "gardenio-coopmux-ipwhitelist", ...]
	middlewareNames []string
	logger         *slog.Logger
	mu             sync.Mutex
}

// BouncerConfig holds configuration for the Bouncer.
type BouncerConfig struct {
	Client          dynamic.Interface
	Namespace       string
	MiddlewareNames []string
	Logger          *slog.Logger
}

// NewBouncer creates a new Bouncer.
func NewBouncer(cfg BouncerConfig) *Bouncer {
	return &Bouncer{
		client:          cfg.Client,
		namespace:       cfg.Namespace,
		middlewareNames: cfg.MiddlewareNames,
		logger:          cfg.Logger,
	}
}

// ListIPs returns the current sourceRange from the first managed middleware.
func (g *Bouncer) ListIPs(ctx context.Context) ([]string, error) {
	if len(g.middlewareNames) == 0 {
		return nil, fmt.Errorf("no middleware names configured")
	}
	mw, err := g.client.Resource(middlewareGVR).Namespace(g.namespace).Get(ctx, g.middlewareNames[0], metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get middleware %s: %w", g.middlewareNames[0], err)
	}
	return extractSourceRange(mw), nil
}

// AddIP adds a CIDR to all managed middlewares' sourceRange.
func (g *Bouncer) AddIP(ctx context.Context, cidr string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	var errs []string
	for _, name := range g.middlewareNames {
		if err := g.addIPToMiddleware(ctx, name, cidr); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// RemoveIP removes a CIDR from all managed middlewares' sourceRange.
func (g *Bouncer) RemoveIP(ctx context.Context, cidr string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	var errs []string
	for _, name := range g.middlewareNames {
		if err := g.removeIPFromMiddleware(ctx, name, cidr); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (g *Bouncer) addIPToMiddleware(ctx context.Context, name, cidr string) error {
	mw, err := g.client.Resource(middlewareGVR).Namespace(g.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}

	existing := extractSourceRange(mw)
	for _, ip := range existing {
		if ip == cidr {
			return nil // already present
		}
	}

	newRange := append(existing, cidr)
	return g.patchSourceRange(ctx, name, newRange)
}

func (g *Bouncer) removeIPFromMiddleware(ctx context.Context, name, cidr string) error {
	mw, err := g.client.Resource(middlewareGVR).Namespace(g.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}

	existing := extractSourceRange(mw)
	var newRange []string
	found := false
	for _, ip := range existing {
		if ip == cidr {
			found = true
			continue
		}
		newRange = append(newRange, ip)
	}
	if !found {
		return fmt.Errorf("%s not in whitelist", cidr)
	}
	if len(newRange) == 0 {
		return fmt.Errorf("cannot remove last IP — whitelist would be empty")
	}

	return g.patchSourceRange(ctx, name, newRange)
}

func (g *Bouncer) patchSourceRange(ctx context.Context, name string, sourceRange []string) error {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"ipWhiteList": map[string]interface{}{
				"sourceRange": sourceRange,
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	_, err = g.client.Resource(middlewareGVR).Namespace(g.namespace).Patch(
		ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("patch %s: %w", name, err)
	}

	g.logger.Info("patched middleware sourceRange", "middleware", name, "sourceRange", sourceRange)
	return nil
}

func extractSourceRange(mw *unstructured.Unstructured) []string {
	spec, ok := mw.Object["spec"].(map[string]interface{})
	if !ok {
		return nil
	}
	ipWL, ok := spec["ipWhiteList"].(map[string]interface{})
	if !ok {
		return nil
	}
	sr, ok := ipWL["sourceRange"].([]interface{})
	if !ok {
		return nil
	}
	var result []string
	for _, v := range sr {
		if s, ok := v.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

// NormalizeCIDR ensures an IP has a /32 suffix if no CIDR notation is provided.
func NormalizeCIDR(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		_, _, err := net.ParseCIDR(s)
		if err != nil {
			return "", fmt.Errorf("invalid CIDR %q: %w", s, err)
		}
		return s, nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return "", fmt.Errorf("invalid IP %q", s)
	}
	return s + "/32", nil
}
