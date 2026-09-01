// Package kubernetes provides the Kubernetes implementation of the sandbox
// runtime contracts. It deliberately owns only Kubernetes resources; it does
// not change the local Docker provider.
package kubernetes

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
)

const (
	DefaultNamespacePrefix       = "sandboxd"
	DefaultRuntimeClass          = "kata-vm-isolation"
	DefaultWorkspaceMount        = "/home/sandbox"
	DefaultWebPort         int32 = 3000
)

// RuntimeProfile restricts the runtime class policy. The local Kind profile is
// deliberately an allow-list of one empty value, not an escape hatch for
// arbitrary runtime classes.
type RuntimeProfile string

const (
	RuntimeProfileProduction RuntimeProfile = "production"
	RuntimeProfileLocalKind  RuntimeProfile = "local-kind"
)

var (
	ErrInvalidConfig = errors.New("kubernetes runtime: invalid configuration")
	imageReference   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@+-]*$`)
)

// ResourcePolicy describes the per-sandbox resource envelope. Requests and
// limits must each define CPU and memory, and the PVC size is deliberately
// separate from container memory.
type ResourcePolicy struct {
	Requests      corev1.ResourceList
	Limits        corev1.ResourceList
	WorkspaceSize resource.Quantity
}

// Timeouts bounds control-plane work. Event streams intentionally have no
// server-imposed deadline: their caller owns the stream context.
type Timeouts struct {
	API     time.Duration
	Exec    time.Duration
	File    time.Duration
	Preview time.Duration
}

// SecurityDefaults are the mandatory pod/container hardening defaults. They
// are configuration so an operator's generated configuration is validated
// before it can weaken the sandbox.
type SecurityDefaults struct {
	RunAsUser                int64
	RunAsGroup               int64
	FSGroup                  int64
	RunAsNonRoot             bool
	ReadOnlyRootFilesystem   bool
	AllowPrivilegeEscalation bool
	DropAllCapabilities      bool
	SeccompProfile           corev1.SeccompProfileType
}

// Config controls all Kubernetes resources created for a sandbox.
type Config struct {
	NamespacePrefix string
	StorageClass    string
	SandboxImage    string
	GitOpsImage     string
	RuntimeClass    string
	RuntimeProfile  RuntimeProfile
	WorkspaceMount  string
	WebPort         int32
	ClusterDomain   string
	// AgentProxyURL is the private control-plane Service used by runtimed
	// tasks. It is intentionally an in-cluster URL, never an external endpoint.
	AgentProxyURL string

	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
	Resources    ResourcePolicy
	Security     SecurityDefaults
	Timeouts     Timeouts

	// MaxFileBytes and MaxFileEntries put provider-side ceilings on requests
	// even if an API caller accidentally supplies a larger per-request bound.
	MaxFileBytes   int64
	MaxFileEntries int
}

// DefaultConfig returns a secure baseline. Operators must supply their storage
// class and sandbox image before constructing an adapter.
func DefaultConfig() Config {
	return Config{
		NamespacePrefix: DefaultNamespacePrefix,
		RuntimeClass:    DefaultRuntimeClass,
		RuntimeProfile:  RuntimeProfileProduction,
		WorkspaceMount:  DefaultWorkspaceMount,
		WebPort:         DefaultWebPort,
		ClusterDomain:   "cluster.local",
		Resources: ResourcePolicy{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
			WorkspaceSize: resource.MustParse("10Gi"),
		},
		Timeouts: Timeouts{
			API:     15 * time.Second,
			Exec:    2 * time.Minute,
			File:    30 * time.Second,
			Preview: 30 * time.Second,
		},
		Security: SecurityDefaults{
			RunAsUser:                1000,
			RunAsGroup:               1000,
			FSGroup:                  1000,
			RunAsNonRoot:             true,
			ReadOnlyRootFilesystem:   true,
			AllowPrivilegeEscalation: false,
			DropAllCapabilities:      true,
			SeccompProfile:           corev1.SeccompProfileTypeRuntimeDefault,
		},
		MaxFileBytes:   8 << 20,
		MaxFileEntries: 1_000,
	}
}

// Validate rejects unsafe or incomplete runtime configuration before any API
// call is made.
func (c Config) Validate() error {
	if errs := kvalidation.IsDNS1123Label(c.NamespacePrefix); len(errs) > 0 {
		return configError("namespace prefix", strings.Join(errs, "; "))
	}
	if errs := kvalidation.IsDNS1123Subdomain(c.StorageClass); c.StorageClass == "" || len(errs) > 0 {
		if c.StorageClass == "" {
			return configError("storage class", "is required")
		}
		return configError("storage class", strings.Join(errs, "; "))
	}
	if !imageReference.MatchString(c.SandboxImage) {
		return configError("sandbox image", "must be a non-empty container image reference")
	}
	if c.GitOpsImage != "" && !imageReference.MatchString(c.GitOpsImage) {
		return configError("git-ops image", "must be a container image reference")
	}
	switch c.RuntimeProfile {
	case "", RuntimeProfileProduction:
		if c.RuntimeClass != DefaultRuntimeClass {
			return configError("runtime class", fmt.Sprintf("must be %q", DefaultRuntimeClass))
		}
	case RuntimeProfileLocalKind:
		if c.RuntimeClass != "" {
			return configError("runtime class", "must be empty for the local Kind runtime profile")
		}
	default:
		return configError("runtime profile", "is unsupported")
	}
	if c.WorkspaceMount != DefaultWorkspaceMount {
		return configError("workspace mount", fmt.Sprintf("must be %q", DefaultWorkspaceMount))
	}
	if c.WebPort != DefaultWebPort {
		// The production Cilium policies admit preview traffic only on this
		// Service port. Rejecting a divergent environment value avoids
		// provisioning sandboxes that the authenticated gateway cannot reach.
		return configError("web port", fmt.Sprintf("must be %d to match the production network policy", DefaultWebPort))
	}
	if errs := kvalidation.IsDNS1123Subdomain(c.ClusterDomain); c.ClusterDomain == "" || len(errs) > 0 {
		if c.ClusterDomain == "" {
			return configError("cluster domain", "is required")
		}
		return configError("cluster domain", strings.Join(errs, "; "))
	}
	if c.AgentProxyURL != "" {
		if err := validateAgentProxyURL(c.AgentProxyURL, c.ClusterDomain); err != nil {
			return configError("agent proxy URL", err.Error())
		}
	}
	for key, value := range c.NodeSelector {
		if errs := kvalidation.IsQualifiedName(key); len(errs) > 0 {
			return configError("node selector key", strings.Join(errs, "; "))
		}
		if errs := kvalidation.IsValidLabelValue(value); value == "" || len(errs) > 0 {
			return configError("node selector value", "must be a valid non-empty label value")
		}
	}
	for _, toleration := range c.Tolerations {
		if err := validateToleration(toleration); err != nil {
			return err
		}
	}
	if err := validateResources(c.Resources); err != nil {
		return err
	}
	if err := validateSecurity(c.Security); err != nil {
		return err
	}
	if err := validateTimeout("API timeout", c.Timeouts.API); err != nil {
		return err
	}

	if err := validateTimeout("exec timeout", c.Timeouts.Exec); err != nil {
		return err
	}
	if err := validateTimeout("file timeout", c.Timeouts.File); err != nil {
		return err
	}
	if err := validateTimeout("preview timeout", c.Timeouts.Preview); err != nil {
		return err
	}
	if c.MaxFileBytes < 1 || c.MaxFileBytes > 64<<20 {
		return configError("maximum file bytes", "must be between 1 byte and 64 MiB")
	}
	if c.MaxFileEntries < 1 || c.MaxFileEntries > 10_000 {
		return configError("maximum file entries", "must be between 1 and 10000")
	}
	return nil
}

func validateSecurity(security SecurityDefaults) error {
	if security.RunAsUser < 1 || security.RunAsGroup < 1 || security.FSGroup < 1 {
		return configError("security identity", "must use positive non-root user, group, and fsGroup IDs")
	}
	if !security.RunAsNonRoot || !security.ReadOnlyRootFilesystem || security.AllowPrivilegeEscalation ||
		!security.DropAllCapabilities || security.SeccompProfile != corev1.SeccompProfileTypeRuntimeDefault {
		return configError("security defaults", "must require non-root, read-only root, no privilege escalation, dropped capabilities, and RuntimeDefault seccomp")
	}
	return nil
}

func configError(field, message string) error {
	return fmt.Errorf("%w: %s %s", ErrInvalidConfig, field, message)
}

func validateToleration(t corev1.Toleration) error {
	if t.Key != "" {
		if errs := kvalidation.IsQualifiedName(t.Key); len(errs) > 0 {
			return configError("toleration key", strings.Join(errs, "; "))
		}
	}
	switch t.Operator {
	case corev1.TolerationOpExists:
		if t.Value != "" {
			return configError("toleration", "with Exists operator cannot have a value")
		}
	case corev1.TolerationOpEqual:
		if t.Key == "" || t.Value == "" {
			return configError("toleration", "with Equal operator requires key and value")
		}
	default:
		return configError("toleration operator", "must be Exists or Equal")
	}
	if t.Effect != "" && t.Effect != corev1.TaintEffectNoSchedule &&
		t.Effect != corev1.TaintEffectPreferNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
		return configError("toleration effect", "is invalid")
	}
	if t.TolerationSeconds != nil {
		if *t.TolerationSeconds < 0 || t.Effect != corev1.TaintEffectNoExecute {
			return configError("toleration seconds", "require a non-negative NoExecute effect")
		}
	}
	return nil
}

func validateAgentProxyURL(raw, clusterDomain string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil {
		return errors.New("must be a valid URL")
	}
	if parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" ||
		parsed.Opaque != "" {
		return errors.New("must be an internal HTTP service URL without path, credentials, query, or fragment")
	}
	host := parsed.Hostname()
	if host == "" || host != strings.ToLower(host) || len(kvalidation.IsDNS1123Subdomain(host)) != 0 {
		return errors.New("must use a lowercase Kubernetes service DNS name")
	}
	if parsed.Port() != "9100" {
		return errors.New("must use private service port 9100")
	}
	suffix := ".svc." + strings.ToLower(clusterDomain)
	if !strings.HasSuffix(host, suffix) {
		return fmt.Errorf("must end in %q", suffix)
	}
	serviceAndNamespace := strings.TrimSuffix(host, suffix)
	if len(strings.Split(serviceAndNamespace, ".")) != 2 {
		return errors.New("must name a service and namespace")
	}
	if _, err := strconv.ParseUint(parsed.Port(), 10, 16); err != nil {
		return errors.New("must use a valid TCP port")
	}
	return nil
}

func validateResources(policy ResourcePolicy) error {
	for _, list := range []struct {
		name string
		list corev1.ResourceList
	}{{"resource requests", policy.Requests}, {"resource limits", policy.Limits}} {
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			value, ok := list.list[name]
			if !ok || value.Sign() <= 0 {
				return configError(list.name, fmt.Sprintf("must define positive %s", name))
			}
		}
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request := policy.Requests[name]
		limit := policy.Limits[name]
		if request.Cmp(limit) > 0 {
			return configError("resource policy", fmt.Sprintf("%s request exceeds limit", name))
		}
	}
	if policy.WorkspaceSize.Sign() <= 0 {
		return configError("workspace size", "must be positive")
	}
	return nil
}

func validateTimeout(name string, timeout time.Duration) error {
	if timeout < time.Second || timeout > time.Hour {
		return configError(name, "must be between one second and one hour")
	}
	return nil
}
