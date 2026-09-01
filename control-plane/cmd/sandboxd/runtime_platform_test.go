package main

import (
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
)

func TestConfiguredPlatformDefaultsToLocal(t *testing.T) {
	platform, err := configuredPlatform(func(string) string { return "" })
	if err != nil {
		t.Fatalf("configuredPlatform: %v", err)
	}
	if platform != platformLocal {
		t.Fatalf("platform = %q, want %q", platform, platformLocal)
	}
}

func TestConfiguredPlatformDoesNotInferKubernetesFromDatabaseURL(t *testing.T) {
	platform, err := configuredPlatform(func(key string) string {
		if key == "DATABASE_URL" {
			return "postgresql://db.example/sandboxd"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("configuredPlatform: %v", err)
	}
	if platform != platformLocal {
		t.Fatalf("platform = %q with only DATABASE_URL, want %q", platform, platformLocal)
	}
}

func TestConfiguredPlatformRejectsUnknownValue(t *testing.T) {
	_, err := configuredPlatform(func(key string) string {
		if key == "SANDBOXD_PLATFORM" {
			return "docker-and-kubernetes"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "SANDBOXD_PLATFORM") {
		t.Fatalf("configuredPlatform error = %v, want platform validation error", err)
	}
}

func TestConfiguredLocalKubernetesStartupRequiresExplicitPlatform(t *testing.T) {
	env := localKubernetesEnvironment()
	env["SANDBOXD_PLATFORM"] = "local"
	_, err := configuredLocalKubernetesStartup(envLookup(env))
	if err == nil || !strings.Contains(err.Error(), "SANDBOXD_PLATFORM=kubernetes-local") {
		t.Fatalf("configuredLocalKubernetesStartup error = %v, want explicit platform requirement", err)
	}
}

func TestConfiguredLocalKubernetesStartupUsesConstrainedKindProfile(t *testing.T) {
	config, err := configuredLocalKubernetesStartup(envLookup(localKubernetesEnvironment()))
	if err != nil {
		t.Fatalf("configuredLocalKubernetesStartup: %v", err)
	}
	if !config.Local || config.Store.Provider != "sqlite" || config.Runtime.RuntimeClass != "" ||
		config.Runtime.RuntimeProfile != "local-kind" || config.PreviewURLScheme != "http" {
		t.Fatalf("local Kubernetes config is not constrained: %#v", config)
	}
}

func TestConfiguredLocalKubernetesStartupRejectsUnsafeRuntimeClass(t *testing.T) {
	env := localKubernetesEnvironment()
	env["SANDBOXD_KUBERNETES_RUNTIME_CLASS"] = "runc"
	_, err := configuredLocalKubernetesStartup(envLookup(env))
	if err == nil || !strings.Contains(err.Error(), "runtime class") {
		t.Fatalf("configuredLocalKubernetesStartup error = %v, want runtime class validation", err)
	}
}

func TestConfiguredLocalKubernetesStartupRejectsAPIKeys(t *testing.T) {
	env := localKubernetesEnvironment()
	env["SANDBOXD_API_TOKENS"] = "legacy:secret"
	_, err := configuredLocalKubernetesStartup(envLookup(env))
	if err == nil || !strings.Contains(err.Error(), "local account") {
		t.Fatalf("configuredLocalKubernetesStartup error = %v, want account-profile validation", err)
	}
}

func TestLocalPlatformRejectsEntraEvenWithDatabaseURL(t *testing.T) {
	env := productionEnvironment()
	delete(env, "SANDBOXD_PLATFORM")
	if err := validateLocalPlatformProfile(envLookup(env)); err == nil ||
		!strings.Contains(err.Error(), "SANDBOXD_PLATFORM=kubernetes") {
		t.Fatalf("validateLocalPlatformProfile error = %v, want explicit platform requirement", err)
	}
}

func TestConfiguredKubernetesStartupRequiresExplicitPlatform(t *testing.T) {
	env := productionEnvironment()
	delete(env, "SANDBOXD_PLATFORM")
	_, err := configuredKubernetesStartup(envLookup(env))
	if err == nil || !strings.Contains(err.Error(), "SANDBOXD_PLATFORM=kubernetes") {
		t.Fatalf("configuredKubernetesStartup error = %v, want explicit platform requirement", err)
	}
}

func TestConfiguredKubernetesStartupRejectsMalformedLifecycleDuration(t *testing.T) {
	env := productionEnvironment()
	env["SANDBOXD_IDLE_REAP_INTERVAL_SECONDS"] = "not-a-duration"
	_, err := configuredKubernetesStartup(envLookup(env))
	if err == nil || !strings.Contains(err.Error(), "SANDBOXD_IDLE_REAP_INTERVAL_SECONDS") {
		t.Fatalf("configuredKubernetesStartup error = %v, want duration validation error", err)
	}
}

func TestConfiguredKubernetesStartupRejectsMissingPostgres(t *testing.T) {
	env := productionEnvironment()
	delete(env, "DATABASE_URL")
	_, err := configuredKubernetesStartup(envLookup(env))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("configuredKubernetesStartup error = %v, want PostgreSQL validation error", err)
	}
}

func TestConfiguredKubernetesStartupRejectsWebPortOutsideNetworkPolicy(t *testing.T) {
	env := productionEnvironment()
	env["SANDBOXD_KUBERNETES_WEB_PORT"] = "4173"
	_, err := configuredKubernetesStartup(envLookup(env))
	if err == nil || !strings.Contains(err.Error(), "web port") {
		t.Fatalf("configuredKubernetesStartup error = %v, want web port validation error", err)
	}
}

func TestConfiguredKubernetesStartupUsesValidatedLifecycleDurations(t *testing.T) {
	env := productionEnvironment()
	env["SANDBOXD_IDLE_THRESHOLD_SECONDS"] = "123"
	env["SANDBOXD_IDLE_REAP_INTERVAL_SECONDS"] = "45"
	env["SANDBOXD_KEEPALIVE_MAX_SECONDS"] = "67"

	config, err := configuredKubernetesStartup(envLookup(env))
	if err != nil {
		t.Fatalf("configuredKubernetesStartup: %v", err)
	}
	if got := config.IdleThreshold.Seconds(); got != 123 {
		t.Fatalf("idle threshold = %v seconds, want 123", got)
	}
	if got := config.IdleReapInterval.Seconds(); got != 45 {
		t.Fatalf("idle reap interval = %v seconds, want 45", got)
	}
	if got := config.KeepaliveMax.Seconds(); got != 67 {
		t.Fatalf("keepalive max = %v seconds, want 67", got)
	}
}

func TestConfiguredKubernetesRuntimeHonorsCustomClusterDomain(t *testing.T) {
	env := productionEnvironment()
	env["SANDBOXD_KUBERNETES_CLUSTER_DOMAIN"] = "kubernetes.internal"
	env["SANDBOXD_AGENT_PROXY_URL"] = "http://agent-proxy.sandboxd-system.svc.kubernetes.internal:9100"

	config, err := configuredKubernetesRuntime(envLookup(env))
	if err != nil {
		t.Fatalf("configuredKubernetesRuntime: %v", err)
	}
	if config.ClusterDomain != "kubernetes.internal" {
		t.Fatalf("cluster domain = %q, want custom domain", config.ClusterDomain)
	}
}

func TestValidateAuthProfileReloadRejectsProfileTransitions(t *testing.T) {
	tests := []struct {
		name    string
		current *auth.Config
		next    *auth.Config
		wantErr bool
	}{
		{
			name:    "local token rotation",
			current: &auth.Config{Profile: auth.ProfileLocal},
			next:    &auth.Config{Profile: auth.ProfileLocal},
		},
		{
			name:    "entra credential rotation",
			current: &auth.Config{Profile: auth.ProfileEntra},
			next:    &auth.Config{Profile: auth.ProfileEntra},
		},
		{
			name:    "local to entra",
			current: &auth.Config{Profile: auth.ProfileLocal},
			next:    &auth.Config{Profile: auth.ProfileEntra},
			wantErr: true,
		},
		{
			name:    "entra to local",
			current: &auth.Config{Profile: auth.ProfileEntra},
			next:    &auth.Config{Profile: auth.ProfileLocal},
			wantErr: true,
		},
		{
			name:    "local to invalid",
			current: &auth.Config{Profile: auth.ProfileLocal},
			next:    &auth.Config{Profile: auth.ProfileInvalid},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAuthProfileReload(test.current, test.next)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateAuthProfileReload() error = %v, want error=%t", err, test.wantErr)
			}
			if test.wantErr && !strings.Contains(err.Error(), "requires restart") {
				t.Fatalf("reload rejection = %v, want restart requirement", err)
			}
		})
	}
}

func productionEnvironment() map[string]string {
	return map[string]string{
		"SANDBOXD_PLATFORM":                 "kubernetes",
		"DATABASE_URL":                      "postgresql://db.example/sandboxd",
		"SANDBOXD_AUTH_PROFILE":             "entra",
		"SANDBOXD_ENTRA_TENANT_ID":          "tenant.example",
		"SANDBOXD_ENTRA_CLIENT_ID":          "client-id",
		"SANDBOXD_ENTRA_CLIENT_SECRET":      "client-secret",
		"SANDBOXD_ENTRA_REDIRECT_URL":       "https://console.example.test/auth/callback",
		"SANDBOXD_KUBERNETES_STORAGE_CLASS": "encrypted-fast",
		"SANDBOXD_KUBERNETES_SANDBOX_IMAGE": "registry.example/sandboxd:latest",
		"SANDBOXD_AGENT_PROXY_URL":          "http://agent-proxy.sandboxd-system.svc.cluster.local:9100",
		"PREVIEW_DOMAIN":                    "preview.example.test",
		"PREVIEW_URL_SCHEME":                "https",
		"SANDBOXD_RUNTIME_LEASE_HOLDER":     "sandboxd-control-plane-abc123",
		"SANDBOXD_PUBLIC_HTTP_PORT":         "443",
	}
}

func localKubernetesEnvironment() map[string]string {
	return map[string]string{
		"SANDBOXD_PLATFORM":                 "kubernetes-local",
		"SANDBOXD_DB":                       "/var/lib/sandboxd/state/sandboxd.db",
		"SANDBOXD_SECRETS_KEY_FILE":         "/var/lib/sandboxd/state/secrets.key",
		"SANDBOXD_AUTH_PROFILE":             "local",
		"SANDBOXD_LOCAL_AUTH_MODE":          "accounts",
		"SANDBOXD_API_AUTH_DISABLED":        "false",
		"SANDBOXD_CONTROL_PLANE_REPLICAS":   "1",
		"SANDBOXD_KUBERNETES_STORAGE_CLASS": "standard",
		"SANDBOXD_KUBERNETES_SANDBOX_IMAGE": "sandboxd-base:kind",
		"SANDBOXD_AGENT_PROXY_URL":          "http://sandboxd-control-plane.sandboxd-system.svc.cluster.local:9100",
		"PREVIEW_DOMAIN":                    "localhost",
		"PREVIEW_URL_SCHEME":                "http",
		"SANDBOXD_PUBLIC_HTTP_PORT":         "9090",
		"SANDBOXD_RUNTIME_LEASE_HOLDER":     "sandboxd-control-plane-kind",
	}
}

func envLookup(values map[string]string) environmentLookup {
	return func(key string) string { return values[key] }
}
