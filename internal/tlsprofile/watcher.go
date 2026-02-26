/*
Copyright 2025.

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

package tlsprofile

import (
	"context"
	"fmt"
	"reflect"

	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// SecurityProfileWatcher watches the APIServer object for TLS profile changes
// and invokes OnProfileChange when the profile changes (e.g. to trigger graceful shutdown).
type SecurityProfileWatcher struct {
	client.Client

	// InitialTLSProfileSpec is the TLS profile spec that was configured when the operator started.
	InitialTLSProfileSpec configv1.TLSProfileSpec

	// OnProfileChange is called when the TLS profile changes. Common use is to cancel the main context
	// so the operator restarts and picks up the new profile.
	OnProfileChange func(ctx context.Context, oldSpec, newSpec configv1.TLSProfileSpec)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SecurityProfileWatcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("tlssecurityprofilewatcher").
		For(&configv1.APIServer{}, builder.WithPredicates(
			predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool {
					return e.Object.GetName() == APIServerName
				},
				UpdateFunc: func(e event.UpdateEvent) bool {
					return e.ObjectNew.GetName() == APIServerName
				},
				DeleteFunc: func(e event.DeleteEvent) bool {
					return e.Object.GetName() == APIServerName
				},
				GenericFunc: func(e event.GenericEvent) bool {
					return e.Object.GetName() == APIServerName
				},
			},
		)).
		Complete(r)
}

// Reconcile watches for changes to the APIServer TLS profile and invokes OnProfileChange when it changes.
func (r *SecurityProfileWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx, "name", req.Name)

	apiServer := &configv1.APIServer{}
	if err := r.Get(ctx, req.NamespacedName, apiServer); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get APIServer %s: %w", req.NamespacedName.String(), err)
	}

	currentSpec, err := GetTLSProfileSpec(apiServer.Spec.TLSSecurityProfile)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get TLS profile from APIServer %s: %w", req.NamespacedName.String(), err)
	}

	if !reflect.DeepEqual(r.InitialTLSProfileSpec, currentSpec) {
		if r.OnProfileChange != nil {
			logger.Info("TLS profile changed, invoking callback", "old", r.InitialTLSProfileSpec.MinTLSVersion, "new", currentSpec.MinTLSVersion)
			r.OnProfileChange(ctx, r.InitialTLSProfileSpec, currentSpec)
		}
		r.InitialTLSProfileSpec = currentSpec
	}

	return ctrl.Result{}, nil
}
