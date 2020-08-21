/*
 * This file is part of the KubeVirt project
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
 *
 * Copyright 2020 Red Hat, Inc.
 *
 */

package network

import (
	"fmt"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/kubevirt/tests"
	"kubevirt.io/kubevirt/tests/flags"
	"kubevirt.io/kubevirt/tests/libvmi"
)

var _ = SIGDescribe("Services", func() {
	var virtClient kubecli.KubevirtClient

	runTCPClientExpectingHelloWorldFromServer := func(host, port, namespace string) *batchv1.Job {
		job := tests.NewHelloWorldJob(host, port)
		job, err := virtClient.BatchV1().Jobs(namespace).Create(job)
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		return job
	}

	exposeExistingVMISpec := func(vmi *v1.VirtualMachineInstance, subdomain string, hostname string, selectorLabelKey string, selectorLabelValue string) *v1.VirtualMachineInstance {
		vmi.Labels = map[string]string{selectorLabelKey: selectorLabelValue}
		vmi.Spec.Subdomain = subdomain
		vmi.Spec.Hostname = hostname

		return vmi
	}

	readyVMI := func(vmi *v1.VirtualMachineInstance) *v1.VirtualMachineInstance {
		_, err := virtClient.VirtualMachineInstance(tests.NamespaceTestDefault).Create(vmi)
		Expect(err).ToNot(HaveOccurred())

		return tests.WaitUntilVMIReady(vmi, tests.LoggedInCirrosExpecter)
	}

	cleanupVMI := func(virtClient kubecli.KubevirtClient, vmi *v1.VirtualMachineInstance) {
		if vmi != nil {
			By("Deleting the VMI")
			Expect(virtClient.VirtualMachineInstance(tests.NamespaceTestDefault).Delete(vmi.GetName(), &k8smetav1.DeleteOptions{})).To(Succeed())

			By("Waiting for the VMI to be gone")
			Eventually(func() error {
				_, err := virtClient.VirtualMachineInstance(tests.NamespaceTestDefault).Get(vmi.GetName(), &k8smetav1.GetOptions{})
				return err
			}, 2*time.Minute, time.Second).Should(SatisfyAll(HaveOccurred(), WithTransform(errors.IsNotFound, BeTrue())), "The VMI should be gone within the given timeout")
		}
	}

	assertConnectionToExposedServiceSucceeds := func(serviceName, namespace string, servicePort int) {
		serviceFQDN := fmt.Sprintf("%s.%s", serviceName, namespace)

		By(fmt.Sprintf("starting a job which tries to reach the vmi via service %s", serviceFQDN))
		job := runTCPClientExpectingHelloWorldFromServer(serviceFQDN, strconv.Itoa(servicePort), namespace)

		By(fmt.Sprintf("waiting for the job to report a SUCCESSFUL connection attempt to service %s on port %d", serviceFQDN, servicePort))
		tests.WaitForJobToSucceed(&virtClient, job, 90)
	}

	assertConnectionToExposedServiceFails := func(serviceName, namespace string, servicePort int) {
		serviceFQDN := fmt.Sprintf("%s.%s", serviceName, namespace)

		By(fmt.Sprintf("starting a job which tries to reach the vmi via service %s", serviceFQDN))
		job := runTCPClientExpectingHelloWorldFromServer(serviceFQDN, strconv.Itoa(servicePort), namespace)

		By(fmt.Sprintf("waiting for the job to report a FAILED connection attempt to service %s on port %d", serviceFQDN, servicePort))
		tests.WaitForJobToFail(&virtClient, job, 90)
	}

	BeforeEach(func() {
		var err error
		virtClient, err = kubecli.GetKubevirtClient()
		Expect(err).NotTo(HaveOccurred(), "Should successfully initialize an API client")
	})

	Context("bridge interface binding", func() {
		var inboundVMI *v1.VirtualMachineInstance
		var serviceName string

		const (
			selectorLabelKey   = "expose"
			selectorLabelValue = "me"
			servicePort        = 1500
		)

		createVMISpecWithBridgeInterface := func(vmi *v1.VirtualMachineInstance) *v1.VirtualMachineInstance {
			return libvmi.NewCirros(
				libvmi.WithInterface(libvmi.InterfaceDeviceWithBridgeBinding()),
				libvmi.WithNetwork(v1.DefaultPodNetwork()))
		}

		createReadyVMIWithBridgeBindingAndExposedService := func(hostname string, subdomain string) *v1.VirtualMachineInstance {
			return readyVMI(
				exposeExistingVMISpec(
					createVMISpecWithBridgeInterface(nil), subdomain, hostname, selectorLabelKey, selectorLabelValue))
		}

		BeforeEach(func() {
			subdomain := "vmi"
			hostname := "inbound"

			inboundVMI = createReadyVMIWithBridgeBindingAndExposedService(hostname, subdomain)
			tests.StartTCPServer(inboundVMI, servicePort)
		})

		AfterEach(func() {
			cleanupVMI(virtClient, inboundVMI)
		})

		Context("with a service matching the vmi exposed", func() {
			BeforeEach(func() {
				serviceName = "myservice"

				service := buildServiceSpec(serviceName, servicePort, servicePort, selectorLabelKey, selectorLabelValue)
				_, err := virtClient.CoreV1().Services(inboundVMI.Namespace).Create(service)
				Expect(err).ToNot(HaveOccurred())
			})

			AfterEach(func() {
				Expect(virtClient.CoreV1().Services(inboundVMI.Namespace).Delete(serviceName, &k8smetav1.DeleteOptions{})).To(Succeed())
			})

			It("[test_id:1547] should be able to reach the vmi based on labels specified on the vmi", func() {
				assertConnectionToExposedServiceSucceeds(serviceName, inboundVMI.Namespace, servicePort)
			})

			It("[test_id:1548] should fail to reach the vmi if an invalid servicename is used", func() {
				assertConnectionToExposedServiceFails("wrongservice", inboundVMI.Namespace, servicePort)
			})
		})

		Context("with a subdomain and a headless service given", func() {
			BeforeEach(func() {
				serviceName = inboundVMI.Spec.Subdomain

				service := buildHeadlessServiceSpec(serviceName, servicePort, servicePort, selectorLabelKey, selectorLabelValue)
				_, err := virtClient.CoreV1().Services(inboundVMI.Namespace).Create(service)
				Expect(err).ToNot(HaveOccurred())
			})

			AfterEach(func() {
				Expect(virtClient.CoreV1().Services(inboundVMI.Namespace).Delete(serviceName, &k8smetav1.DeleteOptions{})).To(Succeed())
			})

			It("[test_id:1549]should be able to reach the vmi via its unique fully qualified domain name", func() {
				serviceHostnameWithSubdomain := fmt.Sprintf("%s.%s", inboundVMI.Spec.Hostname, inboundVMI.Spec.Subdomain)
				assertConnectionToExposedServiceSucceeds(serviceHostnameWithSubdomain, inboundVMI.Namespace, servicePort)
			})
		})
	})

	Context("Masquerade interface binding", func() {
		var inboundVMI *v1.VirtualMachineInstance
		var serviceName string

		const (
			selectorLabelKey   = "expose"
			selectorLabelValue = "me"
			servicePort        = 1500
		)

		createReadyVMIWithMasqueradeBindingAndExposedService := func(hostname string, subdomain string) *v1.VirtualMachineInstance {
			vmi := libvmi.NewCirros(
				libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
				libvmi.WithNetwork(v1.DefaultPodNetwork()))
			return readyVMI(
				exposeExistingVMISpec(vmi, subdomain, hostname, selectorLabelKey, selectorLabelValue))
		}

		BeforeEach(func() {
			subdomain := "vmi"
			hostname := "inbound"

			inboundVMI = createReadyVMIWithMasqueradeBindingAndExposedService(hostname, subdomain)

			tests.StartTCPServer(inboundVMI, servicePort)
		})

		Context("with a service matching the vmi exposed", func() {
			var isDualStack bool
			var ipv6ServiceName string

			BeforeEach(func() {
				serviceName = "myservice"

				service := buildServiceSpec(serviceName, servicePort, servicePort, selectorLabelKey, selectorLabelValue)
				_, err := virtClient.CoreV1().Services(inboundVMI.Namespace).Create(service)
				Expect(err).ToNot(HaveOccurred())

				isDualStack = flags.IsDualStackedNetworkConfig
				if isDualStack {
					ipv6ServiceName = serviceName + "v6"

					service := buildIPv6ServiceSpec(ipv6ServiceName, servicePort, servicePort, selectorLabelKey, selectorLabelValue)
					_, err := virtClient.CoreV1().Services(inboundVMI.Namespace).Create(service)
					Expect(err).ToNot(HaveOccurred())
				}
			})

			AfterEach(func() {
				Expect(virtClient.CoreV1().Services(inboundVMI.Namespace).Delete(serviceName, &k8smetav1.DeleteOptions{})).To(Succeed())
			})

			AfterEach(func() {
				if isDualStack {
					Expect(virtClient.CoreV1().Services(inboundVMI.Namespace).Delete(ipv6ServiceName, &k8smetav1.DeleteOptions{})).To(Succeed())
				}
			})

			It("should be able to reach the vmi based on labels specified on the vmi", func() {
				assertConnectionToExposedServiceSucceeds(serviceName, inboundVMI.Namespace, servicePort)
				if isDualStack {
					assertConnectionToExposedServiceSucceeds(ipv6ServiceName, inboundVMI.Namespace, servicePort)
				}
			})

			It("should fail to reach the vmi if an invalid servicename is used", func() {
				assertConnectionToExposedServiceFails("wrongservice", inboundVMI.Namespace, servicePort)
			})
		})
	})
})

func buildHeadlessServiceSpec(serviceName string, exposedPort int, portToExpose int, selectorKey string, selectorValue string) *k8sv1.Service {
	service := buildServiceSpec(serviceName, exposedPort, portToExpose, selectorKey, selectorValue)
	service.Spec.ClusterIP = k8sv1.ClusterIPNone
	return service
}

func buildIPv6ServiceSpec(serviceName string, exposedPort int, portToExpose int, selectorKey string, selectorValue string) *k8sv1.Service {
	service := buildServiceSpec(serviceName, exposedPort, portToExpose, selectorKey, selectorValue)
	ipv6Family := k8sv1.IPv6Protocol
	service.Spec.IPFamily = &ipv6Family

	return service
}

func buildServiceSpec(serviceName string, exposedPort int, portToExpose int, selectorKey string, selectorValue string) *k8sv1.Service {
	return &k8sv1.Service{
		ObjectMeta: k8smetav1.ObjectMeta{
			Name: serviceName,
		},
		Spec: k8sv1.ServiceSpec{
			Selector: map[string]string{
				selectorKey: selectorValue,
			},
			Ports: []k8sv1.ServicePort{
				{Protocol: k8sv1.ProtocolTCP, Port: int32(portToExpose), TargetPort: intstr.FromInt(exposedPort)},
			},
		},
	}
}
