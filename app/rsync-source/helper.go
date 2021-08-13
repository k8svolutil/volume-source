package main

import (
	appsv1 "k8s.io/api/apps/v1"
)

func isDeleteRequired(old, new appsv1.Deployment) bool {
	if old.Spec.Replicas != nil && new.Spec.Replicas != nil {
		if *old.Spec.Replicas != *new.Spec.Replicas {
			return true
		}
	}
	if len(old.Spec.Template.Spec.Containers) == 1 && len(new.Spec.Template.Spec.Containers) == 1 {
		if old.Spec.Template.Spec.Containers[0].Image != new.Spec.Template.Spec.Containers[0].Image {
			return true
		}
	} else {
		return true
	}
	return false
}
