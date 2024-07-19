package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"istio.io/api/annotation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/eoinfennessy/istio-multitenancy/api/v1alpha1"
	"github.com/eoinfennessy/istio-multitenancy/pkg/constants"
	pkgerrors "github.com/eoinfennessy/istio-multitenancy/pkg/errors"
)

func (r *ZoneReconciler) reconcileServices(ctx context.Context, z *v1alpha1.Zone) (*ctrl.Result, error) {
	// Get list of Services that should be part of the Zone
	svcs, err := r.listServicesForZone(ctx, z)
	if err != nil {
		z.Status.SetStatusCondition(v1alpha1.ConditionTypeReconciled, metav1.ConditionFalse, v1alpha1.ConditionReasonReconcileError, "Failed to list Services")
		return &ctrl.Result{}, err
	}

	// Update each Service (if required) to include it in the Zone
	ch := make(chan error)
	for _, svc := range svcs {
		go func() {
			ch <- r.includeServiceInZone(ctx, z, svc)
		}()
	}
	for range svcs {
		err = <-ch
		if err != nil {
			if zoneConflictErr, ok := err.(*pkgerrors.ZoneConflictError); ok {
				z.Status.SetStatusCondition(v1alpha1.ConditionTypeReconciled, metav1.ConditionFalse, v1alpha1.ConditionReasonUnreconcilable, zoneConflictErr.Error())
				// We should not requeue because the Zone is currently unreconcilable
				return &ctrl.Result{}, nil
			} else {
				z.Status.SetStatusCondition(v1alpha1.ConditionTypeReconciled, metav1.ConditionFalse, v1alpha1.ConditionReasonReconcileError, "Failed to include Service in Zone")
				return &ctrl.Result{}, err
			}
		}
	}

	// Clean up Services that should no longer be part of the Zone
	if err = r.cleanUpServices(ctx, z, func(service corev1.Service) bool {
		return !slices.Contains(z.Spec.Namespaces, service.GetNamespace())
	}); err != nil {

		return &ctrl.Result{}, err
	}

	return nil, nil
}

// cleanUpServices removes labels and annotations from Services that should no longer be part of the Zone
func (r *ZoneReconciler) cleanUpServices(ctx context.Context, z *v1alpha1.Zone, predicate func(service corev1.Service) bool) error {
	services := corev1.ServiceList{}
	if err := r.List(ctx, &services, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{constants.ZoneLabel: z.Name}),
	}); err != nil {
		z.Status.SetStatusCondition(v1alpha1.ConditionTypeReconciled, metav1.ConditionFalse, v1alpha1.ConditionReasonReconcileError, "Failed to list Services")
		return err
	}

	ch := make(chan error)
	var updatedServiceCount int
	for _, service := range services.Items {
		if predicate(service) {
			updatedServiceCount++
			go func(ch chan error) {
				delete(service.Labels, constants.ZoneLabel)
				delete(service.Annotations, annotation.NetworkingExportTo.Name)
				ch <- r.Update(ctx, &service)
			}(ch)
		}
	}
	for range updatedServiceCount {
		err := <-ch
		if err != nil {
			z.Status.SetStatusCondition(v1alpha1.ConditionTypeReconciled, metav1.ConditionFalse, v1alpha1.ConditionReasonReconcileError, "Failed to update Service")
			return err
		}
	}
	return nil
}

// listServicesForZone returns a slice of Services that should be part of the Zone
func (r *ZoneReconciler) listServicesForZone(ctx context.Context, z *v1alpha1.Zone) ([]corev1.Service, error) {
	servicesLists := make([]*corev1.ServiceList, len(z.Spec.Namespaces))
	ch := make(chan error)
	for i, ns := range z.Spec.Namespaces {
		servicesLists[i] = &corev1.ServiceList{}
		go func(ch chan error) {
			ch <- r.List(ctx, servicesLists[i], &client.ListOptions{Namespace: ns})
		}(ch)
	}
	for range z.Spec.Namespaces {
		err := <-ch
		if err != nil {
			return nil, err
		}
	}

	var serviceCount int
	for _, servicesList := range servicesLists {
		serviceCount += len(servicesList.Items)
	}
	services := make([]corev1.Service, 0, serviceCount)
	for _, serviceList := range servicesLists {
		services = append(services, serviceList.Items...)
	}
	return services, nil
}

// includeServiceInZone sets the labels and annotations of the Service to include it in the Zone, and
// updates the Service if either has changed.
func (r *ZoneReconciler) includeServiceInZone(ctx context.Context, z *v1alpha1.Zone, svc corev1.Service) error {
	log := ctrl.LoggerFrom(ctx)

	var metaChanged bool
	if labelVal, exists := svc.GetLabels()[constants.ZoneLabel]; exists {
		// Return ZoneConflictError if Service is currently part of another Zone
		if labelVal != z.Name {
			err := &pkgerrors.ZoneConflictError{
				Err: errors.New(fmt.Sprintf("Service %s in namespace %s is currently part of zone %s", svc.Name, svc.Namespace, labelVal)),
			}
			return err
		}
	} else {
		svc.SetLabels(map[string]string{constants.ZoneLabel: z.Name})
		metaChanged = true
	}

	exportToAnnotationValue := strings.Join(z.Spec.Namespaces, ",")
	if svc.GetAnnotations()[annotation.NetworkingExportTo.Name] != exportToAnnotationValue {
		svc.SetAnnotations(map[string]string{annotation.NetworkingExportTo.Name: exportToAnnotationValue})
		metaChanged = true
	}

	if metaChanged {
		log.V(1).Info("Updating Service", "namespace", svc.Namespace, "name", svc.Name)
		return r.Update(ctx, &svc)
	}
	return nil
}