package main

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1 "github.com/k8s-volume-copy/types/apis/demo.io/v1"
	"github.com/k8s-volume-copy/types/constant"
)

func getRsyncSourceTemplate(name, hostName string) *internalv1.RsyncSource {
	cr := &internalv1.RsyncSource{
		TypeMeta: metav1.TypeMeta{
			APIVersion: constant.GroupDemoIO + "/" + constant.VersionV1,
			Kind:       constant.RsyncSourceKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				constant.CreatedByLabel: "volume-source-controller",
				constant.NameLabel:      name,
				constant.AppLabel:       name,
			},
		},
		Spec: internalv1.RsyncSourceSpec{
			Image: rsyncDaemonImage,
			Replicas: func() *int32 {
				var i int32 = 1
				return &i
			}(),
			Volume: corev1.Volume{
				Name: "kubelet-pod-dir",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: kubeletPodDirPath,
					},
				},
			},
			Username: "user",
			Password: "pass",
			HostName: hostName,
		},
	}
	return cr
}
