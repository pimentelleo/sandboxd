package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	runtimekubernetes "github.com/tastyeffectco/sandboxd/control-plane/internal/kubernetes"
)

// configuredKubernetesRuntime builds the provider policy exclusively from
// operator-controlled deployment values. The adapter validates the complete
// result before it contacts the Kubernetes API.
func configuredKubernetesRuntime(getenv environmentLookup) (runtimekubernetes.Config, error) {
	return configuredKubernetesRuntimeForProfile(getenv, runtimekubernetes.RuntimeProfileProduction)
}

// configuredLocalKubernetesRuntime shares the in-cluster adapter parser while
// using the narrowly scoped Kind runtime profile. It can only omit a runtime
// class; it cannot select a less isolated arbitrary runtime class.
func configuredLocalKubernetesRuntime(getenv environmentLookup) (runtimekubernetes.Config, error) {
	return configuredKubernetesRuntimeForProfile(getenv, runtimekubernetes.RuntimeProfileLocalKind)
}

func configuredKubernetesRuntimeForProfile(getenv environmentLookup, profile runtimekubernetes.RuntimeProfile) (runtimekubernetes.Config, error) {
	config := runtimekubernetes.DefaultConfig()
	config.RuntimeProfile = profile
	if profile == runtimekubernetes.RuntimeProfileLocalKind {
		config.RuntimeClass = ""
	}
	config.NamespacePrefix = getenvOr(getenv, "SANDBOXD_KUBERNETES_NAMESPACE_PREFIX", config.NamespacePrefix)
	config.StorageClass = strings.TrimSpace(getenv("SANDBOXD_KUBERNETES_STORAGE_CLASS"))
	config.SandboxImage = strings.TrimSpace(getenv("SANDBOXD_KUBERNETES_SANDBOX_IMAGE"))
	config.GitOpsImage = strings.TrimSpace(getenv("SANDBOXD_KUBERNETES_GIT_OPS_IMAGE"))
	config.RuntimeClass = getenvOr(getenv, "SANDBOXD_KUBERNETES_RUNTIME_CLASS", config.RuntimeClass)
	config.WorkspaceMount = getenvOr(getenv, "SANDBOXD_KUBERNETES_WORKSPACE_MOUNT", config.WorkspaceMount)
	config.ClusterDomain = getenvOr(getenv, "SANDBOXD_KUBERNETES_CLUSTER_DOMAIN", config.ClusterDomain)
	config.AgentProxyURL = strings.TrimSpace(getenv("SANDBOXD_AGENT_PROXY_URL"))
	if config.AgentProxyURL == "" {
		return runtimekubernetes.Config{}, fmt.Errorf("SANDBOXD_AGENT_PROXY_URL is required for Kubernetes runtime")
	}

	var err error
	if config.WebPort, err = envInt32(getenv, "SANDBOXD_KUBERNETES_WEB_PORT", config.WebPort); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if config.MaxFileBytes, err = envInt64(getenv, "SANDBOXD_KUBERNETES_MAX_FILE_BYTES", config.MaxFileBytes); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if config.MaxFileEntries, err = envInt(getenv, "SANDBOXD_KUBERNETES_MAX_FILE_ENTRIES", config.MaxFileEntries); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if config.Timeouts.API, err = envDuration(getenv, "SANDBOXD_KUBERNETES_API_TIMEOUT", config.Timeouts.API); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if config.Timeouts.Exec, err = envDuration(getenv, "SANDBOXD_KUBERNETES_EXEC_TIMEOUT", config.Timeouts.Exec); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if config.Timeouts.File, err = envDuration(getenv, "SANDBOXD_KUBERNETES_FILE_TIMEOUT", config.Timeouts.File); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if config.Timeouts.Preview, err = envDuration(getenv, "SANDBOXD_KUBERNETES_PREVIEW_TIMEOUT", config.Timeouts.Preview); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if err := envResource(getenv, "SANDBOXD_KUBERNETES_REQUEST_CPU", config.Resources.Requests, corev1.ResourceCPU); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if err := envResource(getenv, "SANDBOXD_KUBERNETES_REQUEST_MEMORY", config.Resources.Requests, corev1.ResourceMemory); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if err := envResource(getenv, "SANDBOXD_KUBERNETES_LIMIT_CPU", config.Resources.Limits, corev1.ResourceCPU); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if err := envResource(getenv, "SANDBOXD_KUBERNETES_LIMIT_MEMORY", config.Resources.Limits, corev1.ResourceMemory); err != nil {
		return runtimekubernetes.Config{}, err
	}
	if value := strings.TrimSpace(getenv("SANDBOXD_KUBERNETES_WORKSPACE_SIZE")); value != "" {
		quantity, parseErr := resource.ParseQuantity(value)
		if parseErr != nil {
			return runtimekubernetes.Config{}, fmt.Errorf("SANDBOXD_KUBERNETES_WORKSPACE_SIZE: %w", parseErr)
		}
		config.Resources.WorkspaceSize = quantity
	}
	if selector, parseErr := parseNodeSelector(getenv("SANDBOXD_KUBERNETES_NODE_SELECTOR")); parseErr != nil {
		return runtimekubernetes.Config{}, parseErr
	} else {
		config.NodeSelector = selector
	}
	if tolerations, parseErr := parseTolerations(getenv("SANDBOXD_KUBERNETES_TOLERATIONS")); parseErr != nil {
		return runtimekubernetes.Config{}, parseErr
	} else {
		config.Tolerations = tolerations
	}
	if err := config.Validate(); err != nil {
		return runtimekubernetes.Config{}, err
	}
	return config, nil
}

func envResource(getenv environmentLookup, key string, values corev1.ResourceList, name corev1.ResourceName) error {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return nil
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	values[name] = quantity
	return nil
}

func envInt(getenv environmentLookup, key string, fallback int) (int, error) {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return parsed, nil
}

func envInt32(getenv environmentLookup, key string, fallback int32) (int32, error) {
	value, err := envInt(getenv, key, int(fallback))
	if err != nil {
		return 0, err
	}
	if value < -1<<31 || value > 1<<31-1 {
		return 0, fmt.Errorf("%s is outside int32 range", key)
	}
	return int32(value), nil
}

func envInt64(getenv environmentLookup, key string, fallback int64) (int64, error) {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return parsed, nil
}

func envDuration(getenv environmentLookup, key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	return parsed, nil
}

func parseNodeSelector(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	selector := make(map[string]string)
	for _, item := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("SANDBOXD_KUBERNETES_NODE_SELECTOR must be comma-separated key=value pairs")
		}
		if _, exists := selector[key]; exists {
			return nil, fmt.Errorf("SANDBOXD_KUBERNETES_NODE_SELECTOR repeats key %q", key)
		}
		selector[key] = value
	}
	return selector, nil
}

// parseTolerations only accepts a JSON array so production scheduling policy
// stays declarative and an invalid value cannot silently remove the Kata-node
// toleration. Unknown fields are rejected rather than ignored.
func parseTolerations(raw string) ([]corev1.Toleration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.HasPrefix(raw, "[") {
		return nil, fmt.Errorf("SANDBOXD_KUBERNETES_TOLERATIONS must be a JSON array")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var tolerations []corev1.Toleration
	if err := decoder.Decode(&tolerations); err != nil {
		return nil, fmt.Errorf("SANDBOXD_KUBERNETES_TOLERATIONS: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("SANDBOXD_KUBERNETES_TOLERATIONS must contain one JSON value")
	}
	for index := range tolerations {
		if tolerations[index].Operator == "" {
			tolerations[index].Operator = corev1.TolerationOpEqual
		}
	}
	return tolerations, nil
}
