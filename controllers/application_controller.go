/*
Copyright 2020 The Kubernetes Authors.

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

package controllers

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
)

const (
	loggerCtxKey = "logger"
)

// ApplicationReconciler reconciles a Application object
type ApplicationReconciler struct {
	client.Client
	Mapper meta.RESTMapper
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=app.k8s.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=app.k8s.io,resources=applications/status,verbs=get;update;patch

func (r *ApplicationReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	rootCtx := context.Background()
	logger := r.Log.WithValues("application", req.NamespacedName)
	ctx := context.WithValue(rootCtx, loggerCtxKey, logger)

	var app appv1beta1.Application
	err := r.Get(ctx, req.NamespacedName, &app)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, err
	}

	// Application is in the process of being deleted, so no need to do anything.
	if app.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	resources, err := r.fetchComponentListResources(ctx, app.Spec.ComponentGroupKinds, app.Spec.Selector, app.Namespace)
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	if app.Spec.AddOwnerRef {
		ownerRef := metav1.NewControllerRef(&app, appv1beta1.GroupVersion.WithKind("Application"))
		*ownerRef.Controller = false
		if err := r.setOwnerRefForResources(ctx, *ownerRef, resources); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
	}

	objectStatuses := r.objectStatuses(ctx, resources)
	aggReady := aggregateReady(objectStatuses)

	newApplicationStatus := app.Status.DeepCopy()

	newApplicationStatus.ObservedGeneration = app.Generation
	newApplicationStatus.ComponentList = appv1beta1.ComponentList{
		Objects: objectStatuses,
	}

	if aggReady {
		setReadyCondition(newApplicationStatus, "ComponentsReady", "all components ready")
	} else {
		setNotReadyCondition(newApplicationStatus, "ComponentsNotReady", "some components not ready")
	}

	// TODO: Error conditions

	if equality.Semantic.DeepEqual(newApplicationStatus, app.Status) {
		return ctrl.Result{}, nil
	}

	app.Status = *newApplicationStatus
	if err = r.Client.Update(ctx, &app); err != nil {
		return ctrl.Result{Requeue: true}, err
	}
	return ctrl.Result{}, nil
}

func (r *ApplicationReconciler) fetchComponentListResources(ctx context.Context, groupKinds []metav1.GroupKind, selector *metav1.LabelSelector, namespace string) ([]*unstructured.Unstructured, error) {
	logger := getLoggerOrDie(ctx)
	var resources []*unstructured.Unstructured
	for _, gk := range groupKinds {
		mapping, err := r.Mapper.RESTMapping(schema.GroupKind{
			Group: gk.Group,
			Kind:  gk.Kind,
		})
		if err != nil {
			logger.Info("NoMappingForGK", "gk", gk.String())
			continue
		}

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(mapping.GroupVersionKind)
		if err = r.Client.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels(selector.MatchLabels)); err != nil {
			return resources, err
		}

		for _, u := range list.Items {
			resources = append(resources, &u)
		}
	}
	return resources, nil
}

func (r *ApplicationReconciler) setOwnerRefForResources(ctx context.Context, ownerRef metav1.OwnerReference, resources []*unstructured.Unstructured) error {
	logger := getLoggerOrDie(ctx)
	for _, resource := range resources {
		ownerRefs := resource.GetOwnerReferences()
		ownerRefFound := false
		for i, refs := range ownerRefs {
			if ownerRef.Kind == refs.Kind &&
				ownerRef.APIVersion == refs.APIVersion &&
				ownerRef.Name == refs.Name {
				ownerRefFound = true
				if ownerRef.UID != refs.UID {
					ownerRefs[i] = ownerRef
				}
			}
		}

		if !ownerRefFound {
			ownerRefs = append(ownerRefs, ownerRef)
		}
		resource.SetOwnerReferences(ownerRefs)
		err := r.Client.Update(ctx, resource)
		if err != nil {
			// We log this error, but we continue and try to set the ownerRefs on the other resources.
			logger.Error(err, "ErrorSettingOwnerRef", "gvk", resource.GroupVersionKind().String(),
				"namespace", resource.GetNamespace(), "name", resource.GetName())
		}
	}
	return nil
}

func (r *ApplicationReconciler) objectStatuses(ctx context.Context, resources []*unstructured.Unstructured) []appv1beta1.ObjectStatus {
	logger := getLoggerOrDie(ctx)
	var objectStatuses []appv1beta1.ObjectStatus
	for _, resource := range resources {
		os := appv1beta1.ObjectStatus{
			Group: resource.GroupVersionKind().Group,
			Kind:  resource.GetKind(),
			Name:  resource.GetName(),
			Link:  resource.GetSelfLink(),
		}
		s, err := status(resource)
		if err != nil {
			// Just logging the error for now. Not sure if this is the right way to handle it.
			logger.Error(err, "unable to compute status for resource", "gvk", resource.GroupVersionKind().String(),
				"namespace", resource.GetNamespace(), "name", resource.GetName())
			continue
		}
		os.Status = s
		objectStatuses = append(objectStatuses, os)
	}
	return objectStatuses
}

func aggregateReady(objectStatuses []appv1beta1.ObjectStatus) bool {
	for _, os := range objectStatuses {
		if os.Status != StatusReady {
			return false
		}
	}
	return true
}

func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appv1beta1.Application{}).
		Complete(r)
}

func getLoggerOrDie(ctx context.Context) logr.Logger {
	logger, ok := ctx.Value(loggerCtxKey).(logr.Logger)
	if !ok {
		panic("context didn't contain logger")
	}
	return logger
}
