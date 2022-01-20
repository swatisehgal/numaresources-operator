/*
Copyright 2022 The Kubernetes Authors.

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

package install

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	nropv1alpha1 "github.com/openshift-kni/numaresources-operator/api/numaresourcesoperator/v1alpha1"
	"github.com/openshift-kni/numaresources-operator/pkg/status"
	schedutils "github.com/openshift-kni/numaresources-operator/test/e2e/sched/utils"
	e2eclient "github.com/openshift-kni/numaresources-operator/test/utils/clients"
	"github.com/openshift-kni/numaresources-operator/test/utils/crds"
	"github.com/openshift-kni/numaresources-operator/test/utils/objects"
)

const crdName = "numaresourcesschedulers.nodetopology.openshift.io"

var _ = Describe("[Scheduler] install", func() {
	Context("with a running cluster with all the components", func() {
		It("[test_id: 47574] should perform the scheduler deployment and verify the condition is reported as available", func() {
			var err error
			nroSchedObj := objects.TestNROScheduler()

			By(fmt.Sprintf("creating the NRO Scheduler object: %s", nroSchedObj.Name))
			err = e2eclient.Client.Create(context.TODO(), nroSchedObj)
			Expect(err).NotTo(HaveOccurred())

			err = e2eclient.Client.Get(context.TODO(), client.ObjectKeyFromObject(nroSchedObj), nroSchedObj)
			Expect(err).NotTo(HaveOccurred())

			By("checking that the condition Available=true")
			Eventually(func() bool {
				updatedNROObj := &nropv1alpha1.NUMAResourcesScheduler{}
				err := e2eclient.Client.Get(context.TODO(), client.ObjectKeyFromObject(nroSchedObj), updatedNROObj)
				if err != nil {
					klog.Warningf("failed to get the RTE resource: %v", err)
					return false
				}

				cond := status.FindCondition(updatedNROObj.Status.Conditions, status.ConditionAvailable)
				if cond == nil {
					klog.Warningf("missing conditions in %v", updatedNROObj)
					return false
				}

				klog.Infof("condition: %v", cond)

				return cond.Status == metav1.ConditionTrue
			}, 5*time.Minute, 10*time.Second).Should(BeTrue(), "RTE condition did not become available")

			err = e2eclient.Client.Get(context.TODO(), client.ObjectKeyFromObject(nroSchedObj), nroSchedObj)
			Expect(err).NotTo(HaveOccurred())

			By("checking the NumaResourcesScheduler Deployment is correctly deployed")
			const deploymentCheckTimeout = 5 * time.Minute
			const deploymentCheckPollPeriod = 10 * time.Second
			Eventually(func() bool {
				deploy, err := schedutils.GetDeploymentByOwnerReference(nroSchedObj.UID)
				if err != nil {
					klog.Warningf("unable to get deployment by owner reference: %v", err)
					return false
				}

				if deploy.Status.ReadyReplicas != *deploy.Spec.Replicas {
					klog.Warningf("Invalid number of ready replicas: desired: %d, actual: %d", *deploy.Spec.Replicas, deploy.Status.ReadyReplicas)
					return false
				}
				return true
			}, deploymentCheckTimeout, deploymentCheckPollPeriod).Should(BeTrue(), "Deployment Status not OK")

			By("checking the NumaResourcesScheduler CRD is deployed")
			_, err = crds.GetByName(e2eclient.Client, crdName)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})