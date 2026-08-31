package pool

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubernetesGroup maps a control-plane group name to the Kubernetes
// Service whose ready endpoints back it, and the port to reach each
// endpoint on.
type KubernetesGroup struct {
	// Group is the control-plane group name, e.g. "web-tier". Must match
	// what data plane instances subscribe to.
	Group string
	// Namespace is the Kubernetes namespace the Service lives in.
	Namespace string
	// Service is the Kubernetes Service name whose EndpointSlices are
	// read to discover backends.
	Service string
	// Port is the port number to reach each endpoint on. The provider
	// reports "<endpoint-ip>:<port>" as each backend's address.
	Port int
	// Weight is applied to every backend reported for this group. Leave
	// zero to default to 1 (equal weighting).
	Weight int32
}

// endpointsGetter is the narrow slice of the Kubernetes clientset this
// provider actually uses — declared as an interface so Snapshot can be
// unit-tested against a fake client without a real API server.
type endpointsGetter interface {
	ListEndpointSlices(ctx context.Context, namespace, service string) ([]discoveryv1.EndpointSlice, error)
}

// KubernetesProvider implements Provider by reading a Service's
// EndpointSlices (discovery.k8s.io/v1) and reporting each ready endpoint
// as a backend. EndpointSlices are used rather than the legacy Endpoints
// API: they are the current, scalable representation, expose per-endpoint
// readiness/terminating conditions directly, and are what modern clusters
// keep up to date.
//
// Only endpoints whose Ready condition is true (and which are not
// terminating) are reported, so a pod that is starting up, failing its
// readiness probe, or being drained is excluded — the same "only serve
// traffic to things actually ready for it" principle the Azure provider
// applies with running/provisioned state.
//
// Authentication uses in-cluster config when running inside a pod, falling
// back to a kubeconfig file (explicit path, then the KUBECONFIG env /
// ~/.kube/config default) for local development — mirroring how the Azure
// provider's DefaultAzureCredential tries several credential sources in
// order.
type KubernetesProvider struct {
	groups map[string]KubernetesGroup
	client endpointsGetter
}

// NewKubernetesProvider creates a provider backed by one or more Services.
// kubeconfigPath is an optional explicit path to a kubeconfig file; when
// empty the provider uses in-cluster config if available, otherwise the
// standard kubeconfig loading rules (KUBECONFIG env, then ~/.kube/config).
func NewKubernetesProvider(kubeconfigPath string, groups []KubernetesGroup) (*KubernetesProvider, error) {
	cfg, err := buildRESTConfig(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("pool: failed to build Kubernetes client config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("pool: failed to create Kubernetes clientset: %w", err)
	}

	return newKubernetesProviderWithClient(&clientsetEndpoints{cs: clientset}, groups), nil
}

// newKubernetesProviderWithClient is the injectable constructor used by
// both NewKubernetesProvider and the tests.
func newKubernetesProviderWithClient(client endpointsGetter, groups []KubernetesGroup) *KubernetesProvider {
	groupMap := make(map[string]KubernetesGroup, len(groups))
	for _, g := range groups {
		if g.Weight <= 0 {
			g.Weight = 1
		}
		groupMap[g.Group] = g
	}
	return &KubernetesProvider{groups: groupMap, client: client}
}

// buildRESTConfig resolves a *rest.Config: in-cluster first (when running
// as a pod), then a kubeconfig file (explicit path if given, otherwise the
// default loading rules).
func buildRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" {
		if inCluster, err := rest.InClusterConfig(); err == nil {
			return inCluster, nil
		}
		// Not in a cluster (or no service-account token) — fall through to
		// kubeconfig loading below.
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
}

// Groups implements Provider.
func (p *KubernetesProvider) Groups(_ context.Context) ([]string, error) {
	names := make([]string, 0, len(p.groups))
	for name := range p.groups {
		names = append(names, name)
	}
	return names, nil
}

// Snapshot implements Provider. It lists the Service's EndpointSlices and
// reports each ready, non-terminating endpoint address as a backend at the
// group's configured port. One API call per group per reconcile tick
// (paging aside), regardless of endpoint count.
func (p *KubernetesProvider) Snapshot(ctx context.Context, group string) (Snapshot, error) {
	cfg, ok := p.groups[group]
	if !ok {
		return Snapshot{}, fmt.Errorf("pool: unknown group %q", group)
	}

	slices, err := p.client.ListEndpointSlices(ctx, cfg.Namespace, cfg.Service)
	if err != nil {
		return Snapshot{}, fmt.Errorf("pool: failed to list EndpointSlices for service %s/%s: %w", cfg.Namespace, cfg.Service, err)
	}

	// De-duplicate addresses: an endpoint can appear in more than one
	// slice, and a slice can list the same address under multiple ports;
	// we report each ready IP once at the group's configured port.
	seen := make(map[string]bool)
	backends := make([]Backend, 0)
	for _, slice := range slices {
		for _, ep := range slice.Endpoints {
			if !endpointReady(ep) {
				continue
			}
			for _, ip := range ep.Addresses {
				if ip == "" {
					continue
				}
				addr := fmt.Sprintf("%s:%d", ip, cfg.Port)
				if seen[addr] {
					continue
				}
				seen[addr] = true
				backends = append(backends, Backend{Address: addr, Weight: cfg.Weight})
			}
		}
	}

	return Snapshot{Group: group, Backends: backends}, nil
}

// endpointReady reports whether an endpoint should receive traffic: its
// Ready condition must be true (nil is treated as ready per the API's
// documented default) and it must not be terminating.
func endpointReady(ep discoveryv1.Endpoint) bool {
	if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
		return false
	}
	if ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
		return false
	}
	return true
}

// clientsetEndpoints adapts a real *kubernetes.Clientset to the narrow
// endpointsGetter interface, listing a Service's EndpointSlices via the
// well-known "kubernetes.io/service-name" label.
type clientsetEndpoints struct {
	cs kubernetes.Interface
}

func (c *clientsetEndpoints) ListEndpointSlices(ctx context.Context, namespace, service string) ([]discoveryv1.EndpointSlice, error) {
	list, err := c.cs.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + service,
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// parseKubernetesGroups parses a comma-separated list of
// "group:namespace:service:port[:weight]" specs into KubernetesGroup
// values. Exported-ish helper kept in the pool package so both the
// control-plane wiring and tests share one parser.
func ParseKubernetesGroups(spec string) ([]KubernetesGroup, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}

	var groups []KubernetesGroup
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 4 || len(parts) > 5 {
			return nil, fmt.Errorf("invalid k8s-groups entry %q: expected group:namespace:service:port[:weight]", entry)
		}

		portVal, err := strconv.ParseInt(parts[3], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid port in k8s-groups entry %q: %w", entry, err)
		}

		var weight int32 = 1
		if len(parts) == 5 {
			w, err := strconv.ParseInt(parts[4], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid weight in k8s-groups entry %q: %w", entry, err)
			}
			weight = int32(w)
		}

		groups = append(groups, KubernetesGroup{
			Group:     parts[0],
			Namespace: parts[1],
			Service:   parts[2],
			Port:      int(portVal),
			Weight:    weight,
		})
	}

	return groups, nil
}
