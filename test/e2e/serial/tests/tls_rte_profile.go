/*
 * Copyright 2026 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package tests

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	configv1 "github.com/openshift/api/config/v1"
	ctrltls "github.com/openshift/controller-runtime-common/pkg/tls"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/k8stopologyawareschedwg/deployer/pkg/deployer/platform"
	e2etestenv "github.com/k8stopologyawareschedwg/resource-topology-exporter/test/e2e/utils/testenv"

	nropv1 "github.com/openshift-kni/numaresources-operator/api/v1"
	objtls "github.com/openshift-kni/numaresources-operator/pkg/objectupdate/tls"
	e2eclient "github.com/openshift-kni/numaresources-operator/test/internal/clients"
	"github.com/openshift-kni/numaresources-operator/test/internal/configuration"
	"github.com/openshift-kni/numaresources-operator/test/internal/deploy"
	"github.com/openshift-kni/numaresources-operator/test/internal/objects"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	rteMetricsContainerName = "resource-topology-exporter"
	apiServerClusterName    = "cluster"
)

func alternateAPIServerTLSProfile(cur *configv1.TLSSecurityProfile) (configv1.TLSSecurityProfile, bool) {
	if cur != nil && cur.Type == configv1.TLSProfileCustomType {
		return configv1.TLSSecurityProfile{}, false
	}
	if cur != nil && cur.Type == configv1.TLSProfileOldType {
		return configv1.TLSSecurityProfile{
			Type:         configv1.TLSProfileIntermediateType,
			Intermediate: &configv1.IntermediateTLSProfile{},
		}, true
	}
	return configv1.TLSSecurityProfile{
		Type: configv1.TLSProfileOldType,
		Old:  &configv1.OldTLSProfile{},
	}, true
}

func expectedMetricsTLSMinVersion(alt configv1.TLSSecurityProfile) (string, error) {
	spec, err := ctrltls.GetTLSProfileSpec(&alt)
	if err != nil {
		return "", err
	}
	tlsCfgFn, _ := ctrltls.NewTLSConfigFromProfile(spec)
	cfg := &tls.Config{}
	tlsCfgFn(cfg)
	return objtls.NewSettings(cfg).MinVersion, nil
}

func rteContainer(pod *corev1.Pod) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == rteMetricsContainerName {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

func allRunningRTEPodsHaveMinTLSVersion(ctx context.Context, namespace, wantMin string) error {
	sel := fmt.Sprintf("name=%s", e2etestenv.RTELabelName)
	pods, err := e2eclient.K8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return err
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no RTE pods in namespace %q", namespace)
	}
	want := "--metrics-tls-min-version=" + wantMin
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("pod %s/%s not Running yet", p.Namespace, p.Name)
		}
		cnt := rteContainer(p)
		if cnt == nil {
			return fmt.Errorf("pod %s/%s: no container %q", p.Namespace, p.Name, rteMetricsContainerName)
		}
		joined := strings.Join(cnt.Args, " ")
		if !strings.Contains(joined, want) {
			return fmt.Errorf("pod %s/%s: args missing %q (have args len=%d)", p.Namespace, p.Name, want, len(cnt.Args))
		}
	}
	return nil
}

var _ = Describe("[serial] RTE TLS profile propagation", Serial, Label("feature:tls"), func() {
	It("should roll RTE pods when cluster APIServer TLS profile changes", func(ctx context.Context) {
		if configuration.Plat != platform.OpenShift {
			Skip("APIServer TLS profile exists only on OpenShift")
		}

		Expect(e2eclient.ClientsEnabled).To(BeTrue())

		apiSrv := &configv1.APIServer{}
		err := e2eclient.Client.Get(ctx, client.ObjectKey{Name: apiServerClusterName}, apiSrv)
		if err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				Skip(fmt.Sprintf("cannot read apiservers.config.openshift.io/cluster: %v", err))
			}
			Expect(err).NotTo(HaveOccurred())
		}

		var saved *configv1.TLSSecurityProfile
		if apiSrv.Spec.TLSSecurityProfile != nil {
			saved = apiSrv.Spec.TLSSecurityProfile.DeepCopy()
		}

		DeferCleanup(func() {
			cleanupCtx := context.Background()
			err := wait.PollUntilContextTimeout(cleanupCtx, 10*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
				cur := &configv1.APIServer{}
				if err := e2eclient.Client.Get(ctx, client.ObjectKey{Name: apiServerClusterName}, cur); err != nil {
					klog.ErrorS(err, "TLS profile cleanup: get APIServer")
					return false, nil
				}
				if saved == nil {
					cur.Spec.TLSSecurityProfile = nil
				} else {
					cur.Spec.TLSSecurityProfile = saved.DeepCopy()
				}
				if err := e2eclient.Client.Update(ctx, cur); err != nil {
					klog.ErrorS(err, "TLS profile cleanup: update APIServer")
					return false, nil
				}
				return true, nil
			})
			if err != nil {
				klog.ErrorS(err, "TLS profile cleanup failed; cluster APIServer tlsSecurityProfile may need manual restore")
			}
		})

		alt, ok := alternateAPIServerTLSProfile(apiSrv.Spec.TLSSecurityProfile)
		if !ok {
			Skip("Skipping when APIServer uses a Custom TLS profile")
		}
		wantMin, err := expectedMetricsTLSMinVersion(alt)
		Expect(err).NotTo(HaveOccurred())
		Expect(wantMin).NotTo(BeEmpty())

		nropObj := &nropv1.NUMAResourcesOperator{}
		Expect(e2eclient.Client.Get(ctx, objects.NROObjectKey(), nropObj)).To(Succeed())
		Expect(nropObj.Status.DaemonSets).ToNot(BeEmpty(), "NRO must report an RTE DaemonSet")
		rteNS := nropObj.Status.DaemonSets[0].Namespace

		podBefore, err := deploy.FindNUMAResourcesOperatorPod(ctx, e2eclient.Client, nropObj)
		Expect(err).NotTo(HaveOccurred())
		uidBefore := podBefore.UID

		apiSrv.Spec.TLSSecurityProfile = alt.DeepCopy()
		Expect(e2eclient.Client.Update(ctx, apiSrv)).To(Succeed(), "failed to update APIServer tlsSecurityProfile")

		Eventually(func(g Gomega) {
			podAfter, err := deploy.FindNUMAResourcesOperatorPod(ctx, e2eclient.Client, nropObj)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(podAfter.UID).NotTo(Equal(uidBefore), "operator pod should restart after TLS profile change")
		}).WithTimeout(15 * time.Minute).WithPolling(15 * time.Second).Should(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(allRunningRTEPodsHaveMinTLSVersion(ctx, rteNS, wantMin)).To(Succeed())
		}).WithTimeout(15 * time.Minute).WithPolling(15 * time.Second).Should(Succeed(),
			"RTE DaemonSet should roll so metrics TLS flags match the new cluster TLS profile")
	})
})
