package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
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
	kubeClient        *kubernetes.Clientset
	dynamicClient     dynamic.Interface
	rsyncSourceLister dynamiclister.Lister
	rsyncSourceSynced cache.InformerSynced
	nodeLister        corelisters.NodeLister
	nodeSynced        cache.InformerSynced
	workqueue         workqueue.RateLimitingInterface
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

	informerFactory := informers.NewSharedInformerFactory(kubeClient, 30*time.Second)
	nodeInformer := informerFactory.Core().V1().Nodes().Informer()

	dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 30*time.Second)
	rsyncSourceInformer := dynamicInformerFactory.ForResource(rsyncSourceGVR).Informer()
	c := &controller{
		kubeClient:        kubeClient,
		dynamicClient:     dynamicClient,
		rsyncSourceLister: dynamiclister.New(rsyncSourceInformer.GetIndexer(), rsyncSourceGVR),
		rsyncSourceSynced: rsyncSourceInformer.HasSynced,
		nodeLister:        informerFactory.Core().V1().Nodes().Lister(),
		nodeSynced:        nodeInformer.HasSynced,
		workqueue:         workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
	}

	rsyncSourceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleRsyncSource,
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.handleRsyncSource(newObj)
		},
		DeleteFunc: c.handleRsyncSource,
	})

	nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleNode,
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.handleNode(newObj)
		},
		DeleteFunc: c.handleNode,
	})

	dynamicInformerFactory.Start(stopCh)
	informerFactory.Start(stopCh)
	if err := c.run(stopCh); nil != err {
		klog.Fatalf("Failed to run controller: %v", err)
	}
}

func (c *controller) handleRsyncSource(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
	}
	c.workqueue.Add("rsyncsource/" + key)
}

func (c *controller) handleNode(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
	}
	c.workqueue.Add("node/" + key)
}

func (c *controller) run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.rsyncSourceSynced, c.nodeSynced); !ok {
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
		if len(parts) < 2 {
			utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
			return nil
		}
		if parts[0] == "rsyncsource" {
			if len(parts) != 3 {
				return fmt.Errorf("invalid key %s", key)
			}
			if err := c.syncRsyncSource(context.TODO(), parts[1], parts[2]); err != nil {
				c.workqueue.AddRateLimited(key)
				return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
			}
		}
		if parts[0] == "node" {
			if len(parts) != 2 {
				return fmt.Errorf("invalid key %s", key)
			}
			if err := c.syncNode(context.TODO(), parts[1]); err != nil {
				c.workqueue.AddRateLimited(key)
				return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
			}
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

func (c *controller) syncRsyncSource(ctx context.Context, namespace, name string) error {
	unstruct, err := c.rsyncSourceLister.Namespace(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("RsyncSource '%s/%s` in work queue no longer exists", name, namespace))
			return nil
		}
		return fmt.Errorf("error getting rsync source, error: %s", err)
	}
	rsyncSource := internalv1.RsyncSource{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstruct.UnstructuredContent(),
		&rsyncSource); err != nil {
		return fmt.Errorf("error converting rsync source `%s` in `%s` namespace error: %s",
			unstruct.GetName(), unstruct.GetNamespace(), err)
	}
	if rsyncSource.GetLabels() == nil || rsyncSource.GetLabels()[constant.CreatedByLabel] != "volume-source-controller" {
		return nil
	}
	if rsyncSource.Spec.HostName == "" {
		return nil
	}
	nodeList, err := c.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: constant.K8SIOHostName + "=" + rsyncSource.Spec.HostName,
	})
	if err != nil {
		return err
	}
	if len(nodeList.Items) == 0 {
		return c.ensureRsyncSource(false, namespace, getRsyncSourceTemplate(name, rsyncSource.Spec.HostName))
	}
	return nil
}

func (c *controller) syncNode(ctx context.Context, name string) error {
	node, err := c.nodeLister.Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("node '%s` in work queue no longer exists", name))
			return nil
		}
		return fmt.Errorf("error getting node error: %s", err)
	}
	if node.GetLabels() == nil {
		return fmt.Errorf("error processing node `%s` sync missing node label", node.GetName())
	}
	hostName := node.GetLabels()[constant.K8SIOHostName]
	if node.GetLabels() == nil {
		return fmt.Errorf("error processing node sync missing `%s` kubernetes.io/hostname label", node.GetName())
	}
	return c.ensureRsyncSource(true, namespace, getRsyncSourceTemplate(node.GetName(), hostName))
}

/*
if found and not created by the populator then return error
if want and found return nil
if !want and !found return nil
if want and !found -> create return error/nil
if !want and found -> delete return error/nil
*/
func (c *controller) ensureRsyncSource(want bool, namespace string, rsyncSource *internalv1.RsyncSource) error {
	found := true
	rsyncSourceClone := rsyncSource.DeepCopy()
	populatorMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&rsyncSourceClone)
	if err != nil {
		return err
	}
	rsyncSourceCloneUnstruct := &unstructured.Unstructured{
		Object: populatorMap,
	}
	obj, err := c.dynamicClient.Resource(rsyncSourceGVR).Namespace(namespace).
		Get(context.TODO(), rsyncSourceClone.GetName(), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			found = false
		} else {
			return err
		}
	}
	if found && (obj.GetLabels() == nil || obj.GetLabels()[constant.CreatedByLabel] != "volume-source-controller") {
		return fmt.Errorf("resource found but not created by this operator")
	}
	if want && found {
		return nil
	}
	if !want && !found {
		return nil
	}
	if want && !found {
		_, err := c.dynamicClient.Resource(rsyncSourceGVR).Namespace(namespace).
			Create(context.TODO(), rsyncSourceCloneUnstruct, metav1.CreateOptions{})
		return err
	}
	if !want && found {
		err := c.dynamicClient.Resource(rsyncSourceGVR).Namespace(namespace).
			Delete(context.TODO(), rsyncSourceClone.GetName(), metav1.DeleteOptions{})
		return err
	}
	return nil
}
