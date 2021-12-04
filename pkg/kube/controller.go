package kube

import (
	"context"
	"strings"

	"github.com/rueian/zenvoy/pkg/logger"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	contourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	"github.com/rueian/zenvoy/pkg/alloc"
	"github.com/rueian/zenvoy/pkg/xds"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(contourv1.AddToScheme(scheme))
}

func NewManager(namespace string) (manager.Manager, error) {
	conf, err := config.GetConfig()
	if err != nil {
		return nil, err
	}
	return manager.New(conf, manager.Options{Scheme: scheme, Namespace: namespace})
}

func SetupEndpointController(mgr manager.Manager, monitor *xds.MonitorServer, logger *logger.Std, snapshot *xds.Snapshot, proxyIP string, portMin, portMax uint32) error {
	controller := &EndpointController{
		Client:   mgr.GetClient(),
		Logger:   logger,
		monitor:  monitor,
		snapshot: snapshot,
		portsMap: alloc.NewKeys(portMin, portMax),
		proxyIP:  proxyIP,
	}
	return builder.ControllerManagedBy(mgr).
		For(&v1.Endpoints{}).
		Complete(controller)
}

type EndpointController struct {
	client.Client
	Logger   *logger.Std
	snapshot *xds.Snapshot
	monitor  *xds.MonitorServer
	portsMap *alloc.Keys
	proxyIP  string
}

func (c *EndpointController) setEndpoints(svc, prefix string, fqdn []string, available []xds.Endpoint, count int) {
	c.monitor.TrackCluster(svc, count)
	c.snapshot.SetCluster(svc)
	c.snapshot.SetClusterRoute(svc, fqdn, prefix)
	c.snapshot.SetClusterEndpoints(svc, available...)
}

func (c *EndpointController) removeEndpoints(svc string) {
	c.monitor.RemoveCluster(svc)
	c.portsMap.Release(svc)
	c.snapshot.RemoveClusterRoute(svc)
	c.snapshot.RemoveClusterEndpoints(svc)
	c.snapshot.RemoveCluster(svc)
}

func (c *EndpointController) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {

	def := &contourv1.HTTPProxy{}
	if err := c.Get(ctx, req.NamespacedName, def); err != nil {
		if apierrors.IsNotFound(err) {
			c.removeEndpoints(req.Name)
			return reconcile.Result{}, nil
		}
		c.Logger.Errorf("Fail to get HTTPProxy %s %s: %v", req.Namespace, req.Name, err)
		return reconcile.Result{}, err
	}

	if def.Spec.VirtualHost == nil || def.Spec.VirtualHost.Fqdn == "" {
		c.removeEndpoints(req.Name)
		return reconcile.Result{}, nil
	}

	endpoints := &v1.Endpoints{}
	if err := c.Get(ctx, req.NamespacedName, endpoints); err != nil {
		if apierrors.IsNotFound(err) {
			c.removeEndpoints(req.Name)
			return reconcile.Result{}, nil
		} else {
			c.Logger.Errorf("Fail to get Endpoints %s %s: %v", req.Namespace, req.Name, err)
			return reconcile.Result{}, err
		}
	}

	var count int
	var available []xds.Endpoint
	for _, sub := range endpoints.Subsets {
		port := c.findEndpointPort(endpoints, sub)
		for _, addr := range sub.Addresses {
			available = append(available, xds.Endpoint{IP: addr.IP, Port: uint32(port)})
		}
		count += len(sub.Addresses) + len(sub.NotReadyAddresses)
	}
	if len(available) == 0 {
		port, err := c.portsMap.Acquire(req.Name)
		if err != nil {
			c.Logger.Errorf("Fail to acquire port: %v", err)
			return reconcile.Result{Requeue: true}, nil
		}
		available = append(available, xds.Endpoint{IP: c.proxyIP, Port: port})
	}
	c.setEndpoints(req.Name, "", []string{
		def.Spec.VirtualHost.Fqdn,
		strings.ReplaceAll(def.Spec.VirtualHost.Fqdn, ".dcard.", ".dtto."),
	}, available, count)
	return reconcile.Result{}, nil
}

func (c *EndpointController) findEndpointPort(endpoints *v1.Endpoints, subset v1.EndpointSubset) int32 {
	if len(subset.Ports) == 1 {
		return subset.Ports[0].Port
	}
	for _, port := range subset.Ports {
		switch strings.ToLower(port.Name) {
		case "http", "https", "http2", "tcp":
			return port.Port
		}
	}
	for _, port := range subset.Ports {
		switch port.Protocol {
		case "TCP":
			return port.Port
		}
	}
	return 0
}
