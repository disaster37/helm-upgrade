// Package helmx wraps the Helm SDK with the small surface this CLI needs.
package helmx

import (
	"fmt"
	"os"
	"sync"

	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// Client owns the Helm settings, the OCI registry client and a cache of
// per-namespace action configurations.
type Client struct {
	getter   genericclioptions.RESTClientGetter
	settings *cli.EnvSettings
	registry *registry.Client
	driver   string

	mu      sync.Mutex
	configs map[string]*action.Configuration
}

// NewClient builds a Client. The registry client is created eagerly so that
// `oci://` chart references resolve using ~/.config/helm/registry/config.json
// (populated by `helm registry login`).
func NewClient(getter genericclioptions.RESTClientGetter, settings *cli.EnvSettings) (*Client, error) {
	regOpts := []registry.ClientOption{
		registry.ClientOptDebug(settings.Debug),
		registry.ClientOptEnableCache(true),
		registry.ClientOptWriter(os.Stderr),
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	}
	rc, err := registry.NewClient(regOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating OCI registry client: %w", err)
	}

	driver := os.Getenv("HELM_DRIVER")
	if driver == "" {
		driver = "secret"
	}
	logrus.WithField("driver", driver).Debug("helm storage driver")

	return &Client{
		getter:   getter,
		settings: settings,
		registry: rc,
		driver:   driver,
		configs:  map[string]*action.Configuration{},
	}, nil
}

// Settings exposes the Helm environment settings.
func (c *Client) Settings() *cli.EnvSettings { return c.settings }

// ActionConfig returns (and caches) the Helm action configuration for a
// namespace. An empty namespace means cluster-wide, used by `list -A`.
func (c *Client) ActionConfig(namespace string) (*action.Configuration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if cfg, ok := c.configs[namespace]; ok {
		return cfg, nil
	}

	cfg := new(action.Configuration)
	if err := cfg.Init(c.getter, namespace, c.driver, DebugLog); err != nil {
		return nil, fmt.Errorf("initialising helm for namespace %q: %w", namespace, err)
	}
	cfg.RegistryClient = c.registry

	c.configs[namespace] = cfg
	return cfg, nil
}

// SetActionConfig installs a pre-built action configuration for a namespace,
// bypassing cluster initialisation. It exists so that tests can inject a fake
// (memory driver + fake kube client) configuration.
func (c *Client) SetActionConfig(namespace string, cfg *action.Configuration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configs[namespace] = cfg
}

// DebugLog bridges Helm's action.DebugLog signature into logrus at debug level.
func DebugLog(format string, v ...interface{}) {
	logrus.Debugf(format, v...)
}
