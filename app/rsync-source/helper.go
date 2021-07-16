package main

import (
	corev1 "k8s.io/api/core/v1"
)

func isRestartRequired(old, new corev1.Pod) bool {
	if old.Status.Phase == corev1.PodFailed || old.Status.Phase == corev1.PodSucceeded {
		return true
	}
	if len(old.Spec.Containers) == 1 && len(new.Spec.Containers) == 1 {
		if old.Spec.Containers[0].Image != new.Spec.Containers[0].Image {
			return true
		}
	} else {
		return true
	}
	return false
}
