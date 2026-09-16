// Package kube builds the Kubernetes client configuration shared by every Helm
// action, using the standard client-go loading rules.
package kube

import (
	"fmt"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
)

// Options carries the connection flags that influence kubeconfig loading.
type Options struct {
	KubeConfig string
	Context    string
	Namespace  string
}

// Getter wraps genericclioptions.ConfigFlags, which already implements Helm's
// RESTClientGetter interface (kubeconfig precedence, $KUBECONFIG, --context and
// in-cluster fallback are all handled by client-go).
type Getter struct {
	*genericclioptions.ConfigFlags
}

// NewGetter builds a RESTClientGetter from the supplied options.
func NewGetter(o Options) *Getter {
	f := genericclioptions.NewConfigFlags(true)
	if o.KubeConfig != "" {
		f.KubeConfig = &o.KubeConfig
	}
	if o.Context != "" {
		f.Context = &o.Context
	}
	if o.Namespace != "" {
		f.Namespace = &o.Namespace
	}
	return &Getter{ConfigFlags: f}
}

// DefaultNamespace returns the namespace from the kubeconfig current context,
// or "default" when none is set.
func (g *Getter) DefaultNamespace() string {
	ns, _, err := g.ToRawKubeConfigLoader().Namespace()
	if err != nil || ns == "" {
		return "default"
	}
	return ns
}

// CheckConnectivity performs a single discovery call so that authentication and
// reachability problems surface immediately with an actionable message rather
// than as a raw 401 in the middle of an upgrade loop.
func (g *Getter) CheckConnectivity() error {
	restCfg, err := g.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("creating discovery client: %w", err)
	}
	v, err := dc.ServerVersion()
	if err != nil {
		if errors.IsUnauthorized(err) {
			return fmt.Errorf("cluster rejected the credentials (401) for %s: the token is missing or expired; run `oc login` (OpenShift) or refresh your kubeconfig credentials: %w", restCfg.Host, err)
		}
		if errors.IsForbidden(err) {
			return fmt.Errorf("cluster refused the request (403) for %s: the current user lacks permission to query the server version: %w", restCfg.Host, err)
		}
		return fmt.Errorf("cannot reach the Kubernetes API at %s: %w", restCfg.Host, err)
	}
	logrus.WithFields(logrus.Fields{
		"server":  restCfg.Host,
		"version": v.GitVersion,
	}).Debug("connected to cluster")
	return nil
}
