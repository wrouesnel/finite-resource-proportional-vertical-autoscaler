package frpva

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/go-logr/zapr"
	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	logutil "github.com/wrouesnel/go.logutil"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/apis/v1alpha1"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/controller"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/proportional"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/snapshot"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/webhook"
)

// Webhook certificate modes.
const (
	CertModeSelfManaged = "self-managed"
	CertModeExternal    = "external"
)

var errNoNamespace = errors.New("the operator namespace must be set with --namespace or POD_NAMESPACE")

// RunCmd runs the operator: the admission webhook on every replica, and the rebalancing controller
// on the elected leader.
type RunCmd struct {
	Kubeconfig string `env:"KUBECONFIG"    help:"Path to a kubeconfig. Only required if running out-of-cluster."`
	Namespace  string `env:"POD_NAMESPACE" help:"Namespace the operator runs in. It is never managed."`
	PolicyName string `default:"default"   help:"Name of the ProportionalResourcePolicy to act on"`

	MetricsBindAddress     string `default:":8080" help:"Address the metrics endpoint binds to. Set to 0 to disable."`
	HealthProbeBindAddress string `default:":8081" help:"Address the health probe endpoint binds to"`

	LeaderElect       bool   `default:"true"                                  help:"Elect a leader to run the rebalancing controller"                negatable:""`
	LeaderElectionID  string `default:"frpva.wrouesnel.github.com"            help:"Name of the leader election lease"`
	WebhookPort       int    `default:"9443"                                  help:"Port the admission webhook listens on"`
	WebhookCertDir    string `default:"/tmp/k8s-webhook-server/serving-certs" help:"Directory holding the webhook tls.crt and tls.key"`
	WebhookCertMode   string `default:"self-managed"                          enum:"self-managed,external"                                           help:"self-managed: generate and rotate the webhook certificate and inject its CA. external: use certificates provided in the cert dir (e.g. by cert-manager)."`
	WebhookSecretName string `default:"frpva-webhook-cert"                    help:"Secret holding the self-managed webhook certificate"`
	WebhookService    string `default:"frpva-webhook"                         help:"Service name of the webhook, for the self-managed certificate"`
	WebhookConfigName string `default:"frpva"                                 help:"MutatingWebhookConfiguration to inject the self-managed CA into"`
}

// Scheme returns the scheme of all types the operator uses.
func Scheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("building scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("building scheme: %w", err)
	}
	return scheme, nil
}

func (r *RunCmd) restConfig() (*rest.Config, error) {
	if r.Kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", r.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("loading kubeconfig: %w", err)
		}
		return cfg, nil
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubernetes client configuration: %w", err)
	}
	return cfg, nil
}

// Run is invoked by kong.
func (r *RunCmd) Run(ctx context.Context, _ *EntrypointConfig) error {
	l := logutil.FromCtx(ctx)
	ctrl.SetLogger(zapr.NewLogger(l))

	if r.Namespace == "" {
		return errNoNamespace
	}
	cfg, err := r.restConfig()
	if err != nil {
		return err
	}
	return r.run(ctx, cfg)
}

func (r *RunCmd) run(ctx context.Context, cfg *rest.Config) error {
	l := logutil.FromCtx(ctx)
	scheme, err := Scheme()
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: r.MetricsBindAddress},
		HealthProbeBindAddress:  r.HealthProbeBindAddress,
		LeaderElection:          r.LeaderElect,
		LeaderElectionID:        r.LeaderElectionID,
		LeaderElectionNamespace: r.Namespace,
		Cache: cache.Options{
			// Every pod in the cluster is cached, so don't hold their managed fields too.
			DefaultTransform: cache.TransformStripManagedFields(),
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}: {Namespaces: map[string]cache.Config{r.Namespace: {}}},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	loader := &snapshot.Loader{
		Reader:            mgr.GetCache(),
		PolicyName:        r.PolicyName,
		OperatorNamespace: r.Namespace,
		Entries:           proportional.NewEntryCache(),
	}

	rebalancer := &controller.Rebalancer{
		Client:   mgr.GetClient(),
		Loader:   loader,
		Recorder: mgr.GetEventRecorder("frpva"),
	}
	if err := rebalancer.SetupWithManager(mgr); err != nil {
		return err
	}

	certsReady := make(chan struct{})
	switch r.WebhookCertMode {
	case CertModeSelfManaged:
		err := rotator.AddRotator(mgr, &rotator.CertRotator{
			SecretKey:      types.NamespacedName{Namespace: r.Namespace, Name: r.WebhookSecretName},
			CertDir:        r.WebhookCertDir,
			CAName:         "frpva-ca",
			CAOrganization: "frpva",
			DNSName:        fmt.Sprintf("%s.%s.svc", r.WebhookService, r.Namespace),
			ExtraDNSNames: []string{
				r.WebhookService,
				fmt.Sprintf("%s.%s", r.WebhookService, r.Namespace),
				fmt.Sprintf("%s.%s.svc.cluster.local", r.WebhookService, r.Namespace),
			},
			IsReady:  certsReady,
			Webhooks: []rotator.WebhookInfo{{Name: r.WebhookConfigName, Type: rotator.Mutating}},
		})
		if err != nil {
			return fmt.Errorf("setting up webhook certificate rotation: %w", err)
		}
	default:
		close(certsReady)
	}

	server := ctrlwebhook.NewServer(ctrlwebhook.Options{Port: r.WebhookPort, CertDir: r.WebhookCertDir})
	server.Register(webhook.Path, &ctrlwebhook.Admission{Handler: &webhook.PodMutator{
		Loader:  loader,
		Decoder: admission.NewDecoder(scheme),
	}})
	// The webhook can only be served once its certificate exists, and is only useful once the
	// cache has synced.
	if err := mgr.Add(&deferredRunnable{ready: certsReady, cache: mgr.GetCache(), runnable: server}); err != nil {
		return fmt.Errorf("adding webhook server: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("webhook", server.StartedChecker()); err != nil {
		return fmt.Errorf("adding readiness check: %w", err)
	}

	l.Info("Starting operator",
		zap.String("namespace", r.Namespace),
		zap.String("policy", r.PolicyName),
		zap.String("webhook_address", net.JoinHostPort("", strconv.Itoa(r.WebhookPort))),
		zap.String("cert_mode", r.WebhookCertMode))
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running manager: %w", err)
	}
	return nil
}

// deferredRunnable starts a runnable on every replica once ready is closed and the cache has synced.
type deferredRunnable struct {
	ready    <-chan struct{}
	cache    cache.Cache
	runnable manager.Runnable
}

// Start implements manager.Runnable.
func (d *deferredRunnable) Start(ctx context.Context) error {
	select {
	case <-d.ready:
	case <-ctx.Done():
		return nil
	}
	if !d.cache.WaitForCacheSync(ctx) {
		return nil
	}
	//nolint:wrapcheck
	return d.runnable.Start(ctx)
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (d *deferredRunnable) NeedLeaderElection() bool {
	return false
}
