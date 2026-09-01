package main

import (
	"fmt"
	"strings"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
)

// deploymentPlatform is intentionally explicit. In particular, a database URL
// alone must never switch a Docker deployment into the production runtime.
type deploymentPlatform string

const (
	platformLocal           deploymentPlatform = "local"
	platformKubernetes      deploymentPlatform = "kubernetes"
	platformKubernetesLocal deploymentPlatform = "kubernetes-local"
)

func configuredPlatform(getenv environmentLookup) (deploymentPlatform, error) {
	switch strings.ToLower(strings.TrimSpace(getenv("SANDBOXD_PLATFORM"))) {
	case "", string(platformLocal):
		return platformLocal, nil
	case string(platformKubernetes):
		return platformKubernetes, nil
	case string(platformKubernetesLocal):
		return platformKubernetesLocal, nil
	default:
		return "", fmt.Errorf("SANDBOXD_PLATFORM must be %q, %q, or %q", platformLocal, platformKubernetes, platformKubernetesLocal)
	}
}

// validateLocalPlatformProfile rejects the production auth profile before the
// local startup path derives or creates host-backed state. A DATABASE_URL is
// deliberately not considered here: local deployments retain SQLite/Docker
// behavior unless the platform itself is explicitly selected.
func validateLocalPlatformProfile(getenv environmentLookup) error {
	if auth.ParseConfig(getenv).Profile == auth.ProfileEntra {
		return fmt.Errorf("SANDBOXD_AUTH_PROFILE=entra requires SANDBOXD_PLATFORM=kubernetes")
	}
	return nil
}

// validateAuthProfileReload keeps listener dispatch immutable. hostDispatch
// captures its production-preview handler at startup, so changing profiles on
// SIGHUP could otherwise leave preview traffic on the wrong auth path.
func validateAuthProfileReload(current, next *auth.Config) error {
	currentProfile := authProfile(current)
	nextProfile := authProfile(next)
	if currentProfile != nextProfile {
		return fmt.Errorf("authentication profile transition from %q to %q requires restart", currentProfile, nextProfile)
	}
	return nil
}

func authProfile(config *auth.Config) auth.Profile {
	if config == nil || config.Profile == "" {
		return auth.ProfileLocal
	}
	return config.Profile
}
