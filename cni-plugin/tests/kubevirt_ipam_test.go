// Copyright (c) 2015-2020 Tigera, Inc. All rights reserved.

package main_test

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/projectcalico/calico/cni-plugin/internal/pkg/testutils"
	libapiv3 "github.com/projectcalico/calico/libcalico-go/lib/apis/v3"
	client "github.com/projectcalico/calico/libcalico-go/lib/clientv3"
	"github.com/projectcalico/calico/libcalico-go/lib/names"
	"github.com/projectcalico/calico/libcalico-go/lib/options"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeVirt VM-based handle ID tests
// These tests verify that virt-launcher pods use VM-based handle IDs for IP persistence.
// The Makefile installs minimal KubeVirt CRDs (without operators) before running tests.
// Tests will skip gracefully if CRD installation fails.
var _ = Describe("KubeVirt VM-based handle ID", func() {
	// Skip these tests if not running against Kubernetes datastore
	// since we need to create CRD resources
	if os.Getenv("DATASTORE_TYPE") != "kubernetes" {
		return
	}

	cniVersion := os.Getenv("CNI_SPEC_VERSION")
	calicoClient, err := client.NewFromEnv()
	Expect(err).NotTo(HaveOccurred())

	var k8sClient *kubernetes.Clientset
	var testNs string
	var vmName string
	var vmiName string
	var podName string
	var vmUID string
	var vmiUID string
	var cid string

	BeforeEach(func() {
		testutils.WipeDatastore()
		testutils.MustCreateNewIPPool(calicoClient, "192.168.0.0/16", false, false, true)
		testutils.MustCreateNewIPPool(calicoClient, "fd80:24e2:f998:72d6::/64", false, false, true)

		// Create a unique container ID for each test
		cid = uuid.NewString()

		// Create the node for these tests. The IPAM code requires a corresponding Calico node to exist.
		var name string
		if testutils.IsRunningOnKind() {
			name = os.Getenv("HOSTNAME")
		} else {
			name, _ = names.Hostname()
		}
		testutils.MustCreateNodeWithIPv4Address(calicoClient, name, "10.0.0.1/24")

		// Get Kubernetes client
		config, err := clientcmd.BuildConfigFromFlags("", "/home/user/certs/kubeconfig")
		Expect(err).NotTo(HaveOccurred())
		k8sClient, err = kubernetes.NewForConfig(config)
		Expect(err).NotTo(HaveOccurred())

		// Create a test namespace
		testNs = "test-kubevirt-" + uuid.NewString()[:8]
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: testNs,
			},
		}
		_, err = k8sClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		vmName = "test-vm"
		vmiName = "test-vm" // VMI has same name as VM
		podName = "virt-launcher-" + vmiName + "-abcde"
	})

	AfterEach(func() {
		// Clean up test namespace
		if k8sClient != nil && testNs != "" {
			err := k8sClient.CoreV1().Namespaces().Delete(context.Background(), testNs, metav1.DeleteOptions{})
			if err != nil {
				fmt.Printf("Warning: failed to delete test namespace %s: %v\n", testNs, err)
			}
		}
	})

	// Helper to create VM resource
	createVM := func() {
		config, err := clientcmd.BuildConfigFromFlags("", "/home/user/certs/kubeconfig")
		Expect(err).NotTo(HaveOccurred())

		dynamicClient, err := dynamic.NewForConfig(config)
		Expect(err).NotTo(HaveOccurred())

		vm := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kubevirt.io/v1",
				"kind":       "VirtualMachine",
				"metadata": map[string]interface{}{
					"name":      vmName,
					"namespace": testNs,
				},
				"spec": map[string]interface{}{
					"running": true,
				},
			},
		}

		gvr := schema.GroupVersionResource{
			Group:    "kubevirt.io",
			Version:  "v1",
			Resource: "virtualmachines",
		}

		createdVM, err := dynamicClient.Resource(gvr).Namespace(testNs).Create(context.Background(), vm, metav1.CreateOptions{})
		if err != nil {
			Skip(fmt.Sprintf("Skipping KubeVirt tests - CRDs not installed: %v", err))
		}

		// Get the actual UID assigned by Kubernetes
		vmUID = string(createdVM.GetUID())
		fmt.Printf("[TEST] VM created successfully: %s/%s (actual UID from K8s: %s)\n", testNs, vmName, vmUID)
	}

	// Helper to create VMI resource
	createVMI := func(withMigration bool, migrationUID string) {
		config, err := clientcmd.BuildConfigFromFlags("", "/home/user/certs/kubeconfig")
		Expect(err).NotTo(HaveOccurred())

		dynamicClient, err := dynamic.NewForConfig(config)
		Expect(err).NotTo(HaveOccurred())

		controllerTrue := true
		blockOwnerDeletion := true

		vmiObj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kubevirt.io/v1",
				"kind":       "VirtualMachineInstance",
				"metadata": map[string]interface{}{
					"name":      vmiName,
					"namespace": testNs,
					"ownerReferences": []interface{}{
						map[string]interface{}{
							"apiVersion":         "kubevirt.io/v1",
							"kind":               "VirtualMachine",
							"name":               vmName,
							"uid":                vmUID,
							"controller":         controllerTrue,
							"blockOwnerDeletion": blockOwnerDeletion,
						},
					},
				},
				"spec": map[string]interface{}{},
				"status": map[string]interface{}{
					"activePods": map[string]interface{}{
						"pod-" + uuid.NewString(): "node1",
					},
				},
			},
		}

		// Add migration state if requested
		if withMigration {
			status := vmiObj.Object["status"].(map[string]interface{})
			status["migrationState"] = map[string]interface{}{
				"migrationUID": migrationUID,
				"sourcePod":    "virt-launcher-source",
				"targetPod":    podName,
			}
		}

		gvr := schema.GroupVersionResource{
			Group:    "kubevirt.io",
			Version:  "v1",
			Resource: "virtualmachineinstances",
		}

		createdVMI, err := dynamicClient.Resource(gvr).Namespace(testNs).Create(context.Background(), vmiObj, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Get the actual UID assigned by Kubernetes (it ignores the UID we set)
		vmiUID = string(createdVMI.GetUID())
		fmt.Printf("[TEST] VMI created successfully: %s/%s (actual UID from K8s: %s, withMigration: %v)\n", testNs, vmiName, vmiUID, withMigration)

		// Wait for VMI to be retrievable (to avoid race with IPAM plugin query)
		fmt.Printf("[TEST] Waiting for VMI to be retrievable...\n")
		Eventually(func() error {
			_, err := dynamicClient.Resource(gvr).Namespace(testNs).Get(context.Background(), vmiName, metav1.GetOptions{})
			return err
		}, "5s", "100ms").Should(BeNil())
		fmt.Printf("[TEST] VMI is now retrievable: %s/%s with UID: %s\n", testNs, vmiName, vmiUID)
	}

	// Helper to create virt-launcher pod
	createVirtLauncherPod := func(withMigrationLabel bool, migrationUID string) {
		controllerTrue := true

		podLabels := map[string]string{
			"kubevirt.io/domain": vmiName,
		}

		// Add migration label if this is a target pod
		if withMigrationLabel {
			podLabels["kubevirt.io/migration-target-pod"] = migrationUID
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: testNs,
				UID:       k8stypes.UID("pod-" + uuid.NewString()),
				Labels:    podLabels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "kubevirt.io/v1",
						Kind:       "VirtualMachineInstance",
						Name:       vmiName,
						UID:        k8stypes.UID(vmiUID),
						Controller: &controllerTrue,
					},
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "compute",
						Image: "registry.k8s.io/pause:3.1",
					},
				},
			},
		}

		_, err := k8sClient.CoreV1().Pods(testNs).Create(context.Background(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		fmt.Printf("[TEST] Virt-launcher pod created successfully: %s/%s (withMigrationLabel: %v, VMI UID: %s)\n", testNs, podName, withMigrationLabel, vmiUID)
	}

	// Helper to create VMIM resource
	createVMIM := func(migrationUID string) string {
		config, err := clientcmd.BuildConfigFromFlags("", "/home/user/certs/kubeconfig")
		Expect(err).NotTo(HaveOccurred())

		dynamicClient, err := dynamic.NewForConfig(config)
		Expect(err).NotTo(HaveOccurred())

		vmim := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kubevirt.io/v1",
				"kind":       "VirtualMachineInstanceMigration",
				"metadata": map[string]interface{}{
					"name":      "test-migration",
					"namespace": testNs,
					// Don't set UID - Kubernetes will assign it
				},
				"spec": map[string]interface{}{
					"vmiName": vmiName,
				},
			},
		}

		gvr := schema.GroupVersionResource{
			Group:    "kubevirt.io",
			Version:  "v1",
			Resource: "virtualmachineinstancemigrations",
		}

		createdVMIM, err := dynamicClient.Resource(gvr).Namespace(testNs).Create(context.Background(), vmim, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Get the actual UID assigned by Kubernetes
		actualMigrationUID := string(createdVMIM.GetUID())
		fmt.Printf("[TEST] VMIM created successfully: %s/test-migration (actual UID from K8s: %s)\n", testNs, actualMigrationUID)

		// Wait for VMIM to be retrievable (to avoid race with IPAM plugin query)
		fmt.Printf("[TEST] Waiting for VMIM to be retrievable...\n")
		Eventually(func() error {
			_, err := dynamicClient.Resource(gvr).Namespace(testNs).Get(context.Background(), "test-migration", metav1.GetOptions{})
			return err
		}, "5s", "100ms").Should(BeNil())
		fmt.Printf("[TEST] VMIM is now retrievable: %s/test-migration with UID: %s\n", testNs, actualMigrationUID)

		// Return the actual UID for use in pod labels
		return actualMigrationUID
	}

	It("should use VM-based handle ID for virt-launcher pod", func() {
		fmt.Println("\n[TEST] ===== Running test: should use VM-based handle ID for virt-launcher pod =====")
		// Create VM, VMI, and virt-launcher pod
		createVM()
		createVMI(false, "")
		createVirtLauncherPod(false, "")

		netconf := fmt.Sprintf(`{
			"cniVersion": "%s",
			"name": "net1",
			"type": "calico",
			"etcd_endpoints": "http://%s:2379",
			"kubernetes": {
				"kubeconfig": "/home/user/certs/kubeconfig",
				"k8s_api_root": "https://127.0.0.1:6443"
			},
			"datastore_type": "kubernetes",
			"log_level": "debug",
			"ipam": {
				"type": "calico-ipam"
			}
		}`, cniVersion, os.Getenv("ETCD_IP"))

		// Set CNI_ARGS with pod info (including K8S_POD_INFRA_CONTAINER_ID)
		cniArgs := fmt.Sprintf("K8S_POD_NAME=%s;K8S_POD_NAMESPACE=%s;K8S_POD_INFRA_CONTAINER_ID=%s", podName, testNs, cid)

		// Run IPAM ADD
		result, errOut, exitCode := testutils.RunIPAMPlugin(netconf, "ADD", cniArgs, cid, cniVersion)
		Expect(exitCode).To(Equal(0), fmt.Sprintf("IPAM ADD failed: %v", errOut))
		Expect(result.IPs).To(HaveLen(1))

		// Verify routes are populated for normal pod
		verifyRoutesPopulatedInResult(result, true)

		// Clean up
		_, _, exitCode = testutils.RunIPAMPlugin(netconf, "DEL", cniArgs, cid, cniVersion)
		Expect(exitCode).To(Equal(0))
	})

	Context("Migration target pod", func() {
		var migrationUID string
		var sourcePodName string
		var sourceCID string

		BeforeEach(func() {
			fmt.Printf("\n[TEST] ===== BeforeEach for Migration target pod =====\n")

			// Step 1: Create VM and VMI (initially without migration)
			createVM()
			createVMI(false, "")

			// Step 2: Create source pod and allocate IP to it
			sourcePodName = "virt-launcher-" + vmiName + "-source"
			sourceCID = uuid.NewString()
			createVirtLauncherPod(false, "") // Create the source pod (no migration label)

			// Step 3: Allocate IP to source pod
			fmt.Printf("[TEST] Allocating IP to source pod %s\n", sourcePodName)
			netconf := fmt.Sprintf(`{
				"cniVersion": "%s",
				"name": "net1",
				"type": "calico",
				"etcd_endpoints": "http://%s:2379",
				"kubernetes": {
					"kubeconfig": "/home/user/certs/kubeconfig",
					"k8s_api_root": "https://127.0.0.1:6443"
				},
				"datastore_type": "kubernetes",
				"log_level": "debug",
				"ipam": {
					"type": "calico-ipam"
				}
			}`, cniVersion, os.Getenv("ETCD_IP"))

			sourceCNIArgs := fmt.Sprintf("K8S_POD_NAME=%s;K8S_POD_NAMESPACE=%s;K8S_POD_INFRA_CONTAINER_ID=%s", sourcePodName, testNs, sourceCID)
			result, errOut, exitCode := testutils.RunIPAMPlugin(netconf, "ADD", sourceCNIArgs, sourceCID, cniVersion)
			Expect(exitCode).To(Equal(0), fmt.Sprintf("Source pod IPAM ADD failed: %v", errOut))
			fmt.Printf("[TEST] Source pod allocated IP: %s\n", result.IPs[0].Address.IP.String())

			// Step 4: Now simulate migration - create VMIM and update pod to be target
			placeholderMigrationUID := "migration-" + uuid.NewString()
			migrationUID = createVMIM(placeholderMigrationUID)

			// Step 5: Create target pod with migration label
			podName = "virt-launcher-" + vmiName + "-target" // Different pod name for target
			createVirtLauncherPod(true, migrationUID)

			fmt.Println("[TEST] BeforeEach completed - source pod has IP, target pod created")
		})

		AfterEach(func() {
			// Clean up source pod IP allocation
			if sourcePodName != "" && sourceCID != "" {
				netconf := fmt.Sprintf(`{
					"cniVersion": "%s",
					"name": "net1",
					"type": "calico",
					"etcd_endpoints": "http://%s:2379",
					"kubernetes": {
						"kubeconfig": "/home/user/certs/kubeconfig",
						"k8s_api_root": "https://127.0.0.1:6443"
					},
					"datastore_type": "kubernetes",
					"log_level": "debug",
					"ipam": {
						"type": "calico-ipam"
					}
				}`, cniVersion, os.Getenv("ETCD_IP"))

				sourceCNIArgs := fmt.Sprintf("K8S_POD_NAME=%s;K8S_POD_NAMESPACE=%s;K8S_POD_INFRA_CONTAINER_ID=%s", sourcePodName, testNs, sourceCID)
				testutils.RunIPAMPlugin(netconf, "DEL", sourceCNIArgs, sourceCID, cniVersion)
			}
		})

		It("should return empty routes for migration target pod", func() {
			fmt.Println("\n[TEST] ===== Running test: should return empty routes for migration target pod =====")
			netconf := fmt.Sprintf(`{
				"cniVersion": "%s",
				"name": "net1",
				"type": "calico",
				"etcd_endpoints": "http://%s:2379",
				"kubernetes": {
					"kubeconfig": "/home/user/certs/kubeconfig",
					"k8s_api_root": "https://127.0.0.1:6443"
				},
				"datastore_type": "kubernetes",
				"log_level": "debug",
				"ipam": {
					"type": "calico-ipam"
				}
			}`, cniVersion, os.Getenv("ETCD_IP"))

			// Set CNI_ARGS with pod info (including K8S_POD_INFRA_CONTAINER_ID)
			cniArgs := fmt.Sprintf("K8S_POD_NAME=%s;K8S_POD_NAMESPACE=%s;K8S_POD_INFRA_CONTAINER_ID=%s", podName, testNs, cid)

			// Run IPAM ADD for migration target
			result, errOut, exitCode := testutils.RunIPAMPlugin(netconf, "ADD", cniArgs, cid, cniVersion)
			Expect(exitCode).To(Equal(0), fmt.Sprintf("IPAM ADD failed: %v", errOut))
			Expect(result.IPs).To(HaveLen(1))

			// Verify empty routes for migration target
			verifyRoutesPopulatedInResult(result, false)

			// Clean up
			_, _, exitCode = testutils.RunIPAMPlugin(netconf, "DEL", cniArgs, cid, cniVersion)
			Expect(exitCode).To(Equal(0))
		})

		It("should fail if migration target but KubeVirtVMAddressPersistence is disabled", func() {
			fmt.Println("\n[TEST] ===== Running test: should fail if migration target but KubeVirtVMAddressPersistence is disabled =====")
			// Set IPAMConfig to disable VM address persistence
			ipamConfig := &libapiv3.IPAMConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: "default",
				},
				Spec: libapiv3.IPAMConfigSpec{
					StrictAffinity:               false,
					AutoAllocateBlocks:           true,
					MaxBlocksPerHost:             0,
					KubeVirtVMAddressPersistence: vmAddressPersistencePtr(libapiv3.VMAddressPersistenceDisabled),
				},
			}

			_, err := calicoClient.IPAMConfig().Create(context.Background(), ipamConfig, options.SetOptions{})
			if err != nil {
				// If it already exists, update it
				_, err = calicoClient.IPAMConfig().Update(context.Background(), ipamConfig, options.SetOptions{})
				Expect(err).NotTo(HaveOccurred())
			}

			netconf := fmt.Sprintf(`{
			"cniVersion": "%s",
			"name": "net1",
			"type": "calico",
			"etcd_endpoints": "http://%s:2379",
			"kubernetes": {
				"kubeconfig": "/home/user/certs/kubeconfig",
				"k8s_api_root": "https://127.0.0.1:6443"
			},
			"datastore_type": "kubernetes",
			"log_level": "debug",
			"ipam": {
				"type": "calico-ipam"
			}
		}`, cniVersion, os.Getenv("ETCD_IP"))

			// Set CNI_ARGS with pod info (including K8S_POD_INFRA_CONTAINER_ID)
			cniArgs := fmt.Sprintf("K8S_POD_NAME=%s;K8S_POD_NAMESPACE=%s;K8S_POD_INFRA_CONTAINER_ID=%s", podName, testNs, cid)

			// Run IPAM ADD - should fail
			_, errOut, exitCode := testutils.RunIPAMPlugin(netconf, "ADD", cniArgs, cid, cniVersion)
			Expect(exitCode).NotTo(Equal(0), "IPAM ADD should fail when persistence is disabled")
			Expect(errOut.Msg).To(ContainSubstring("not allowed when KubeVirtVMAddressPersistence is disabled"))

			// Clean up - delete IPAMConfig
			_, err = calicoClient.IPAMConfig().Delete(context.Background(), "default", options.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// Helper function to create VMAddressPersistence pointer
func vmAddressPersistencePtr(v libapiv3.VMAddressPersistence) *libapiv3.VMAddressPersistence {
	return &v
}
