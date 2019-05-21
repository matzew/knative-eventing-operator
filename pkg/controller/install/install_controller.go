package install

import (
	"context"
	"flag"

	mf "github.com/jcrossley3/manifestival"
	eventingv1alpha1 "github.com/openshift-knative/knative-eventing-operator/pkg/apis/eventing/v1alpha1"
	"github.com/openshift-knative/knative-eventing-operator/version"
	"github.com/operator-framework/operator-sdk/pkg/predicate"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	filename = flag.String("filename", "deploy/resources",
		"The filename containing the YAML resources to apply")
	recursive = flag.Bool("recursive", false,
		"If filename is a directory, process all manifests recursively")
	installNs = flag.String("install-ns", "",
		"The namespace in which to create an Install resource, if none exist")
	log = logf.Log.WithName("controller_install")
)

// Add creates a new Install Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	manifest, err := mf.NewManifest(*filename, *recursive, mgr.GetClient())
	if err != nil {
		return err
	}
	return add(mgr, newReconciler(mgr, manifest))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, man mf.Manifest) reconcile.Reconciler {
	return &ReconcileInstall{client: mgr.GetClient(), scheme: mgr.GetScheme(), config: man}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("install-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Install
	err = c.Watch(&source.Kind{Type: &eventingv1alpha1.Install{}}, &handler.EnqueueRequestForObject{}, predicate.GenerationChangedPredicate{})
	if err != nil {
		return err
	}

	// Make an attempt to create an Install CR, if necessary
	if len(*installNs) > 0 {
		c, _ := client.New(mgr.GetConfig(), client.Options{})
		go autoInstall(c, *installNs)
	}
	return nil
}

var _ reconcile.Reconciler = &ReconcileInstall{}

// ReconcileInstall reconciles a Install object
type ReconcileInstall struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
	config mf.Manifest
}

// Reconcile reads that state of the cluster for a Install object and makes changes based on the state read
// and what is in the Install.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileInstall) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Install")

	// Fetch the Install instance
	instance := &eventingv1alpha1.Install{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			r.config.DeleteAll()
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// stages hook for future work (e.g. deleteObsoleteResources)
	stages := []func(*eventingv1alpha1.Install) error{
		r.install,
	}

	for _, stage := range stages {
		if err := stage(instance); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

// Apply the embedded resources
func (r *ReconcileInstall) install(instance *eventingv1alpha1.Install) error {
	// Transform resources as appropriate
	fns := []mf.Transformer{mf.InjectOwner(instance), addSCCforSpecialClusterRoles}
	if len(instance.Spec.Namespace) > 0 {
		fns = append(fns, mf.InjectNamespace(instance.Spec.Namespace))
	}
	r.config.Transform(fns...)

	if instance.Status.Version == version.Version {
		// we've already successfully applied our YAML
		return nil
	}
	// Apply the resources in the YAML file
	if err := r.config.ApplyAll(); err != nil {
		return err
	}

	// Update status
	instance.Status.Resources = r.config.Resources
	instance.Status.Version = version.Version
	if err := r.client.Status().Update(context.TODO(), instance); err != nil {
		return err
	}
	return nil
}

func autoInstall(c client.Client, ns string) (err error) {
	const path = "deploy/crds/eventing_v1alpha1_install_cr.yaml"
	log.Info("Automatic Install requested", "namespace", ns)
	installList := &eventingv1alpha1.InstallList{}
	err = c.List(context.TODO(), &client.ListOptions{Namespace: ns}, installList)
	if err != nil {
		log.Error(err, "Unable to list Installs")
		return err
	}
	if len(installList.Items) == 0 {
		if manifest, err := mf.NewManifest(path, false, c); err == nil {
			if err = manifest.Transform(mf.InjectNamespace(ns)).ApplyAll(); err != nil {
				log.Error(err, "Unable to create Install")
			}
		} else {
			log.Error(err, "Unable to create Install manifest")
		}
	} else {
		log.Info("Install found", "name", installList.Items[0].Name)
	}
	return err
}

func addSCCforSpecialClusterRoles(u *unstructured.Unstructured) *unstructured.Unstructured {

	// these do need some openshift specific SCC
	clusterRoles := []string{
		"eventing-broker-filter",
		"knative-eventing-controller",
		"in-memory-channel-controller",
		"in-memory-channel-dispatcher",
		"eventing-sources-controller",
	}

	matchesClusterRole := func(cr string) bool {
		for _, i := range clusterRoles {
			if cr == i {
				return true
			}
		}
		return false
	}

	// massage the roles that require SCC
	if u.GetKind() == "ClusterRole" && matchesClusterRole(u.GetName()) {
		field, _, _ := unstructured.NestedFieldNoCopy(u.Object, "rules")
		// Required to properly run in OpenShift
		unstructured.SetNestedField(u.Object, append(field.([]interface{}), map[string]interface{}{
			"apiGroups":     []interface{}{"security.openshift.io"},
			"verbs":         []interface{}{"use"},
			"resources":     []interface{}{"securitycontextconstraints"},
			"resourceNames": []interface{}{"privileged", "anyuid"},
		}), "rules")
	}
	return u
}
