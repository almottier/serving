/*
Copyright 2018 Google LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package configuration

import (
	"fmt"
	"time"

	"github.com/elafros/elafros/pkg/controller"

	"github.com/golang/glog"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	buildv1alpha1 "github.com/elafros/elafros/pkg/apis/build/v1alpha1"
	"github.com/elafros/elafros/pkg/apis/ela"
	"github.com/elafros/elafros/pkg/apis/ela/v1alpha1"
	clientset "github.com/elafros/elafros/pkg/client/clientset/versioned"
	informers "github.com/elafros/elafros/pkg/client/informers/externalversions"
	listers "github.com/elafros/elafros/pkg/client/listers/ela/v1alpha1"
)

const controllerAgentName = "configuration-controller"

// Controller implements the controller for Configuration resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	elaclientset  clientset.Interface

	// lister indexes properties about Configuration
	lister listers.ConfigurationLister
	synced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder

	// don't start the workers until revisions cache have been synced
	revisionsSynced cache.InformerSynced
}

// NewController creates a new Configuration controller
//TODO(grantr): somewhat generic (generic behavior)
func NewController(
	kubeclientset kubernetes.Interface,
	elaclientset clientset.Interface,
	kubeInformerFactory kubeinformers.SharedInformerFactory,
	elaInformerFactory informers.SharedInformerFactory,
	config *rest.Config,
	controllerConfig controller.Config) controller.Interface {

	// obtain references to a shared index informer for the Configuration
	// and Revision type.
	informer := elaInformerFactory.Elafros().V1alpha1().Configurations()
	revisionInformer := elaInformerFactory.Elafros().V1alpha1().Revisions()

	// Create event broadcaster
	glog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset:   kubeclientset,
		elaclientset:    elaclientset,
		lister:          informer.Lister(),
		synced:          informer.Informer().HasSynced,
		revisionsSynced: revisionInformer.Informer().HasSynced,
		workqueue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Configurations"),
		recorder:        recorder,
	}

	glog.Info("Setting up event handlers")
	// Set up an event handler for when Configuration resources change
	informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueConfiguration,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueConfiguration(new)
		},
	})

	revisionInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.addRevisionEvent,
		UpdateFunc: controller.updateRevisionEvent,
	})
	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
//TODO(grantr): generic
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting Configuration controller")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.synced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	// Wait for the revisions caches to be synced before starting workers
	glog.Info("Waiting for revisions informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.revisionsSynced); !ok {
		return fmt.Errorf("failed to wait for revisions caches to sync")
	}

	glog.Info("Starting workers")
	// Launch workers to process Configuration resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
//TODO(grantr): generic
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
//TODO(grantr): generic
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Configuration resource to be synced.
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing %q: %v", key, err)
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		glog.Infof("Successfully synced %q", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

// enqueueConfiguration takes a Configuration resource and
// converts it into a namespace/name string which is then put onto the work
// queue. This method should *not* be passed resources of any type other than
// Configuration.
//TODO(grantr): generic
func (c *Controller) enqueueConfiguration(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Configuration
// resource with the current status of the resource.
//TODO(grantr): not generic
func (c *Controller) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the Configuration resource with this namespace/name
	config, err := c.lister.Configurations(namespace).Get(name)
	if err != nil {
		// The resource may no longer exist, in which case we stop
		// processing.
		if errors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("configuration %q in work queue no longer exists", key))
			return nil
		}

		return err
	}
	// Don't modify the informer's copy.
	config = config.DeepCopy()

	// Configuration business logic
	if config.GetGeneration() == config.Status.ObservedGeneration {
		// TODO(vaikas): Check to see if Status.LatestCreatedRevisionName is ready and update Status.LatestReady
		glog.Infof("Skipping reconcile since already reconciled %d == %d",
			config.Spec.Generation, config.Status.ObservedGeneration)
		return nil
	}

	glog.Infof("Running reconcile Configuration for %s\n%+v\n%+v\n",
		config.Name, config, config.Spec.RevisionTemplate)
	spec := config.Spec.RevisionTemplate.Spec
	controllerRef := controller.NewConfigurationControllerRef(config)

	if config.Spec.Build != nil {
		// TODO(mattmoor): Determine whether we reuse the previous build.
		build := &buildv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    config.Namespace,
				GenerateName: fmt.Sprintf("%s-", config.Name),
			},
			Spec: *config.Spec.Build,
		}
		build.OwnerReferences = append(build.OwnerReferences, *controllerRef)
		created, err := c.elaclientset.BuildV1alpha1().Builds(build.Namespace).Create(build)
		if err != nil {
			glog.Errorf("Failed to create Build:\n%+v\n%s", build, err)
			c.recorder.Eventf(config, corev1.EventTypeWarning, "CreationFailed", "Failed to create Build %q: %v", build.Name, err)
			return err
		}
		glog.Infof("Created Build:\n%+v", created.Name)
		c.recorder.Eventf(config, corev1.EventTypeNormal, "Created", "Created Build %q", created.Name)
		spec.BuildName = created.Name
	}

	revName, err := generateRevisionName(config)
	if err != nil {
		return err
	}

	revClient := c.elaclientset.ElafrosV1alpha1().Revisions(config.Namespace)
	created, err := revClient.Get(revName, metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			glog.Errorf("Revisions Get for %q failed: %s", revName, err)
			return err
		}

		rev := &v1alpha1.Revision{
			ObjectMeta: config.Spec.RevisionTemplate.ObjectMeta,
			Spec:       spec,
		}
		// TODO: Should this just use rev.ObjectMeta.GenerateName =
		rev.ObjectMeta.Name = revName
		// Can't generate objects in a different namespace from what the call is made against,
		// so use the namespace of the configuration that's being updated for the Revision being
		// created.
		rev.ObjectMeta.Namespace = config.Namespace

		if rev.ObjectMeta.Labels == nil {
			rev.ObjectMeta.Labels = make(map[string]string)
		}
		rev.ObjectMeta.Labels[ela.ConfigurationLabelKey] = config.Name

		if rev.ObjectMeta.Annotations == nil {
			rev.ObjectMeta.Annotations = make(map[string]string)
		}
		rev.ObjectMeta.Annotations[ela.ConfigurationGenerationAnnotationKey] = fmt.Sprintf("%v", config.Spec.Generation)

		// Delete revisions when the parent Configuration is deleted.
		rev.OwnerReferences = append(rev.OwnerReferences, *controllerRef)

		created, err = revClient.Create(rev)
		if err != nil {
			glog.Errorf("Failed to create Revision:\n%+v\n%s", rev, err)
			c.recorder.Eventf(config, corev1.EventTypeWarning, "CreationFailed", "Failed to create Revision %q: %v", rev.Name, err)
			return err
		}
		c.recorder.Eventf(config, corev1.EventTypeNormal, "Created", "Created Revision %q", rev.Name)
		glog.Infof("Created Revision:\n%+v", created)
	} else {
		glog.Infof("Revision already created %s: %s", created.ObjectMeta.Name, err)
	}
	// Update the Status of the configuration with the latest generation that
	// we just reconciled against so we don't keep generating revisions.
	// Also update the LatestCreatedRevisionName so that we'll know revision to check
	// for ready state so that when ready, we can make it Latest.
	config.Status.LatestCreatedRevisionName = created.ObjectMeta.Name
	config.Status.ObservedGeneration = config.Spec.Generation

	glog.Infof("Updating the configuration status:\n%+v", config)

	if _, err = c.updateStatus(config); err != nil {
		glog.Errorf("Failed to update the configuration %s", err)
		return err
	}

	return nil
}

func generateRevisionName(u *v1alpha1.Configuration) (string, error) {
	// TODO: consider making sure the length of the
	// string will not cause problems down the stack
	if u.Spec.Generation == 0 {
		return "", fmt.Errorf("configuration generation cannot be 0")
	}
	return fmt.Sprintf("%s-%05d", u.Name, u.Spec.Generation), nil
}

func (c *Controller) updateStatus(u *v1alpha1.Configuration) (*v1alpha1.Configuration, error) {
	configClient := c.elaclientset.ElafrosV1alpha1().Configurations(u.Namespace)
	newu, err := configClient.Get(u.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	newu.Status = u.Status

	// TODO: for CRD there's no updatestatus, so use normal update
	return configClient.Update(newu)
	//	return configClient.UpdateStatus(newu)
}

func (c *Controller) addRevisionEvent(obj interface{}) {
	revision := obj.(*v1alpha1.Revision)
	revisionName := revision.Name
	namespace := revision.Namespace
	// Lookup and see if this Revision corresponds to a Configuration that
	// we own and hence the Configuration that created this Revision.
	configName := controller.LookupOwningConfigurationName(revision.OwnerReferences)
	if configName == "" {
		return
	}

	config, err := c.lister.Configurations(namespace).Get(configName)
	if err != nil {
		glog.Errorf("Error fetching configuration '%s/%s' upon revision becoming ready: %v",
			namespace, configName, err)
		return
	}

	if revision.Name != config.Status.LatestCreatedRevisionName {
		// The revision isn't the latest created one, so ignore this event.
		glog.Infof("Revision %q is not the latest created one", revisionName)
		return
	}

	// Don't modify the informer's copy.
	config = config.DeepCopy()

	if !revision.Status.IsReady() {
		glog.Infof("Revision %q of configuration %q is not ready", revisionName, config.Name)

		//add LatestRevision condition to be false with the status from the revision
		c.markConfigurationLatestRevisionStatus(config, revision)

		if _, err := c.updateStatus(config); err != nil {
			glog.Errorf("Error updating configuration '%s/%s': %v",
				namespace, configName, err)
		}
		c.recorder.Eventf(config, corev1.EventTypeNormal, "LatestRevisionUpdate",
			"Latest revision of configuration is not ready")

	} else {
		glog.Infof("Revision %q is ready", revisionName)

		alreadyReady := config.Status.IsReady()
		if !alreadyReady {
			c.markConfigurationReady(config, revision)
		}
		glog.Infof("Setting LatestReadyRevisionName of Configuration %q to revision %q",
			config.Name, revision.Name)
		config.Status.LatestReadyRevisionName = revision.Name

		if _, err := c.updateStatus(config); err != nil {
			glog.Errorf("Error updating configuration '%s/%s': %v",
				namespace, configName, err)
		}
		if !alreadyReady {
			c.recorder.Eventf(config, corev1.EventTypeNormal, "ConfigurationReady",
				"Configuration becomes ready")
		}
		c.recorder.Eventf(config, corev1.EventTypeNormal, "LatestReadyUpdate",
			"LatestReadyRevisionName updated to %q", revision.Name)

	}
	return
}

func (c *Controller) updateRevisionEvent(old, new interface{}) {
	c.addRevisionEvent(new)
}

func getLatestRevisionStatusCondition(revision *v1alpha1.Revision) *v1alpha1.RevisionCondition {
	for _, cond := range revision.Status.Conditions {
		if !(cond.Type == v1alpha1.RevisionConditionReady && cond.Status == corev1.ConditionTrue) {
			return &cond
		}
	}
	return nil
}

// Mark ConfigurationConditionReady of Configuration ready as the given latest
// created revision is ready.
func (c *Controller) markConfigurationReady(
	config *v1alpha1.Configuration, revision *v1alpha1.Revision) {
	glog.Infof("Marking Configuration %q ready", config.Name)
	config.Status.RemoveCondition(v1alpha1.ConfigurationConditionLatestRevisionReady)
	config.Status.SetCondition(
		&v1alpha1.ConfigurationCondition{
			Type:   v1alpha1.ConfigurationConditionReady,
			Status: corev1.ConditionTrue,
			Reason: "LatestRevisionReady",
		})
}

// Mark ConfigurationConditionLatestRevisionReady of Configuration to false with the status
// from the revision
func (c *Controller) markConfigurationLatestRevisionStatus(
	config *v1alpha1.Configuration, revision *v1alpha1.Revision) {
	config.Status.RemoveCondition(v1alpha1.ConfigurationConditionReady)
	cond := getLatestRevisionStatusCondition(revision)
	if cond == nil {
		glog.Infof("Revision status is not updated yet")
		return
	}
	config.Status.SetCondition(
		&v1alpha1.ConfigurationCondition{
			Type:    v1alpha1.ConfigurationConditionLatestRevisionReady,
			Status:  corev1.ConditionFalse,
			Reason:  cond.Reason,
			Message: cond.Message,
		})
}
