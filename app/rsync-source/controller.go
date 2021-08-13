package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/dynamic/dynamiclister"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	internalv1 "github.com/k8s-volume-copy/types/apis/demo.io/v1"
	"github.com/k8s-volume-copy/types/constant"
)

var (
	rsyncSourceGVR = schema.GroupVersionResource{
		Group:    constant.GroupDemoIO,
		Version:  constant.VersionV1,
		Resource: constant.RsyncSourceResource,
	}

	rsyncSourceGK = schema.GroupKind{
		Group: constant.GroupDemoIO,
		Kind:  constant.RsyncSourceKind,
	}
)

type controller struct {
	kubeClient    *kubernetes.Clientset
	dynamicClient dynamic.Interface
	vrLister      dynamiclister.Lister
	vrSynced      cache.InformerSynced
	workqueue     workqueue.RateLimitingInterface
}

func runController(cfg *rest.Config) {
	klog.Infof("Starting controller for %s", strings.ToLower(rsyncSourceGK.String()))
	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		close(stopCh)
		<-sigCh
		os.Exit(1) // second signal. Exit directly.
	}()

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if nil != err {
		klog.Fatalf("Failed to create kube client: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(cfg)
	if nil != err {
		klog.Fatalf("Failed to create dynamic client: %v", err)
	}

	dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, time.Second*30)
	informer := dynamicInformerFactory.ForResource(rsyncSourceGVR).Informer()
	c := &controller{
		kubeClient:    kubeClient,
		dynamicClient: dynamicClient,
		vrLister:      dynamiclister.New(informer.GetIndexer(), rsyncSourceGVR),
		vrSynced:      informer.HasSynced,
		workqueue:     workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handle,
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.handle(newObj)
		},
		DeleteFunc: c.handle,
	})

	dynamicInformerFactory.Start(stopCh)
	if err := c.run(stopCh); nil != err {
		klog.Fatalf("Failed to run controller: %v", err)
	}
}

func (c *controller) handle(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
	}
	c.workqueue.Add(key)
}

func (c *controller) run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.vrSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	go wait.Until(c.runWorker, time.Second, stopCh)
	<-stopCh
	return nil
}

func (c *controller) runWorker() {
	processNext := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		parts := strings.Split(key, "/")
		if len(parts) != 2 {
			utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
			return nil
		}
		if err := c.syncPopulator(context.TODO(), key, parts[0], parts[1]); err != nil {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.workqueue.Forget(obj)
		return nil
	}

	for {
		obj, shutdown := c.workqueue.Get()
		if shutdown {
			return
		}
		if err := processNext(obj); err != nil {
			utilruntime.HandleError(err)
		}
	}
}

func (c *controller) syncPopulator(ctx context.Context, key, namespace, name string) error {
	unstruct, err := c.vrLister.Namespace(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("volume copy '%s' in work queue no longer exists", key))
			return nil
		}
		return fmt.Errorf("error getting volume rename error: %s", err)
	}
	rsyncSource := internalv1.RsyncSource{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstruct.UnstructuredContent(),
		&rsyncSource); err != nil {
		return fmt.Errorf("error converting rsync source `%s` in `%s` namespace error: %s",
			unstruct.GetName(), unstruct.GetNamespace(), err)
	}
	tc, err := templateConfigFromRsyncSource(rsyncSource)
	if err != nil {
		return fmt.Errorf("error creating template config, error : %s", err)
	}
	cmTemplate := tc.getCmTemplate()
	deploymentTemplate := tc.getDeploymentTemplate()
	serviceTemplate := tc.getSvcTemplate()
	delete := rsyncSource.DeletionTimestamp != nil
	if delete {
		if err := c.ensureConfigMap(ctx, false, rsyncSource.GetNamespace(), cmTemplate.DeepCopy()); err != nil {
			klog.Info(*cmTemplate)
			return fmt.Errorf("error ensuring configmap(false) for rsync source `%s` in `%s` namespace error: %s",
				unstruct.GetName(), unstruct.GetNamespace(), err)
		}
		if err := c.ensureDeployment(ctx, false, rsyncSource.GetNamespace(), deploymentTemplate.DeepCopy()); err != nil {
			return fmt.Errorf("error ensuring deploymet(false) for rsync source `%s` in `%s` namespace error: %s",
				unstruct.GetName(), unstruct.GetNamespace(), err)
		}
		if err := c.ensureService(ctx, false, rsyncSource.GetNamespace(), serviceTemplate.DeepCopy()); err != nil {
			return fmt.Errorf("error ensuring service(false) for rsync source `%s` in `%s` namespace error: %s",
				unstruct.GetName(), unstruct.GetNamespace(), err)
		}
		if err := c.ensureRsyncSourceFinalizer(ctx, false, rsyncSource.DeepCopy()); err != nil {
			klog.Error(err)
			return err
		}
		return nil
	}
	if err := c.ensureRsyncSourceFinalizer(ctx, true, rsyncSource.DeepCopy()); err != nil {
		klog.Error(err)
		return err
	}
	if err := c.ensureConfigMap(ctx, true, rsyncSource.GetNamespace(), cmTemplate.DeepCopy()); err != nil {
		klog.Info(*cmTemplate)
		return fmt.Errorf("error ensuring configmap(true) for rsync source `%s` in `%s` namespace error: %s",
			unstruct.GetName(), unstruct.GetNamespace(), err)
	}
	if err := c.ensureDeployment(ctx, true, rsyncSource.GetNamespace(), deploymentTemplate.DeepCopy()); err != nil {
		return fmt.Errorf("error ensuring pod(true) for rsync source `%s` in `%s` namespace error: %s",
			unstruct.GetName(), unstruct.GetNamespace(), err)
	}
	if err := c.ensureService(ctx, true, rsyncSource.GetNamespace(), serviceTemplate.DeepCopy()); err != nil {
		return fmt.Errorf("error ensuring service(true) for rsync source `%s` in `%s` namespace error: %s",
			unstruct.GetName(), unstruct.GetNamespace(), err)
	}
	return nil
}

/*
if found and not created by the populator then return error
if want and found return nil
if !want and !found return nil
if want and !found -> create return error/nil
if !want and found -> delete return error/nil
*/
func (c *controller) ensureDeployment(ctx context.Context, want bool, namespace string, deployment *appsv1.Deployment) error {
	deploymentClone := deployment.DeepCopy()
	found := true
	obj, err := c.kubeClient.AppsV1().Deployments(namespace).
		Get(ctx, deploymentClone.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			found = false
		} else {
			return err
		}
	}
	if found && (obj.GetLabels() == nil || obj.GetLabels()[constant.CreatedByLabel] != constant.ComponentNameRsyncSourceController) {
		return fmt.Errorf("resource found but not created by this operator")
	}
	if want && found {
		if isDeleteRequired(*obj, *deployment) {
			return c.ensureDeployment(ctx, false, namespace, deploymentClone)
		}
		return nil
	}
	if want == found {
		return nil
	}
	if want && !found {
		_, err := c.kubeClient.AppsV1().Deployments(namespace).
			Create(ctx, deploymentClone, metav1.CreateOptions{})
		return err
	}
	if !want && found {
		err := c.kubeClient.AppsV1().Deployments(namespace).
			Delete(ctx, deploymentClone.Name, metav1.DeleteOptions{})
		return err
	}
	return nil
}

/*
if found and not created by the populator then return error
if want and found return nil
if !want and !found return nil
if want and !found -> create return error/nil
if !want and found -> delete return error/nil
*/
func (c *controller) ensureService(ctx context.Context, want bool, namespace string, svc *corev1.Service) error {
	svcClone := svc.DeepCopy()
	found := true
	obj, err := c.kubeClient.CoreV1().Services(namespace).
		Get(ctx, svcClone.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			found = false
		} else {
			return err
		}
	}
	if found && (obj.GetLabels() == nil || obj.GetLabels()[constant.CreatedByLabel] != constant.ComponentNameRsyncSourceController) {
		return fmt.Errorf("resource found but not created by this operator")
	}
	if want == found {
		return nil
	}
	if want && !found {
		_, err := c.kubeClient.CoreV1().Services(namespace).
			Create(ctx, svcClone, metav1.CreateOptions{})
		return err
	}
	if !want && found {
		err := c.kubeClient.CoreV1().Services(namespace).
			Delete(ctx, svcClone.Name, metav1.DeleteOptions{})
		return err
	}
	return nil
}

/*
if found and not created by the populator then return error
if want and found return nil
if !want and !found return nil
if want and !found -> create return error/nil
if !want and found -> delete return error/nil
*/
func (c *controller) ensureConfigMap(ctx context.Context, want bool, namespace string, cm *corev1.ConfigMap) error {
	cmClone := cm.DeepCopy()
	found := true
	obj, err := c.kubeClient.CoreV1().ConfigMaps(namespace).
		Get(ctx, cmClone.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			found = false
		} else {
			return err
		}
	}
	if found && (obj.GetLabels() == nil || obj.GetLabels()[constant.CreatedByLabel] != constant.ComponentNameRsyncSourceController) {
		return fmt.Errorf("resource found but not created by this operator")
	}
	if want == found {
		return nil
	}
	if want && !found {
		_, err := c.kubeClient.CoreV1().ConfigMaps(namespace).
			Create(ctx, cmClone, metav1.CreateOptions{})
		return err
	}
	if !want && found {
		err := c.kubeClient.CoreV1().ConfigMaps(namespace).
			Delete(ctx, cmClone.Name, metav1.DeleteOptions{})
		return err
	}
	return nil
}

// updateVolumeCopy updates a volume copy object
func (c *controller) updateRsyncSource(ctx context.Context, cr *internalv1.RsyncSource) error {
	clone := cr.DeepCopy()
	rsMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(clone)
	if err != nil {
		return err
	}
	rsUnstruct := &unstructured.Unstructured{
		Object: rsMap,
	}
	_, err = c.dynamicClient.Resource(rsyncSourceGVR).Namespace(clone.GetNamespace()).
		Update(ctx, rsUnstruct, metav1.UpdateOptions{})
	return err
}

func (c *controller) ensureRsyncSourceFinalizer(ctx context.Context, want bool, cr *internalv1.RsyncSource) error {
	finalizers := []string{}
	found := false
	for _, v := range cr.GetFinalizers() {
		if v == constant.RsyncSourceProtectionFinalizer {
			found = true
			continue
		}
		finalizers = append(finalizers, v)
	}
	if found == want {
		return nil
	}
	if want {
		finalizers = append(finalizers, constant.RsyncSourceProtectionFinalizer)
	}
	clone := cr.DeepCopy()
	clone.Finalizers = finalizers
	return c.updateRsyncSource(ctx, clone)
}
