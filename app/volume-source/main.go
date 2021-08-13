package main

import (
	"flag"
	"path/filepath"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"
)

var (
	rsyncDaemonImage  string
	kubeletPodDirPath string
	namespace         string
)

func main() {
	klog.InitFlags(nil)
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"),
			"(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.StringVar(&rsyncDaemonImage, "rsync-daemon-image", "ghcr.io/k8svol/rsync-daemon:ci", "Rsync daemon image")
	flag.StringVar(&kubeletPodDirPath, "kubelet-pod-dir-path", "/var/lib/kubelet/pods", "Path of pods folder inside kubelet dir")
	flag.StringVar(&namespace, "namespace", "k8svol", "Namespace of rsync source deployment")
	flag.Parse()

	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			klog.Fatalf("error getting k8s config error: %s", err)
		}
	}
	runController(cfg)
}
