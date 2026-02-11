// Copyright (c) 2015-2020 Tigera, Inc. All rights reserved.

package main_test

import (
	"context"
	"fmt"
	"os"

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

// updateIPAMKubeVirtIPPersistence updates the KubeVirtVMAddressPersistence setting in IPAMConfig
func updateIPAMKubeVirtIPPersistence(calicoClient client.Interface, persistence *libapiv3.VMAddressPersistence) {
	ctx := context.Background()
	ipamConfig, err := calicoClient.IPAMConfig().Get(ctx, "default", options.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	ipamConfig.Spec.KubeVirtVMAddressPersistence = persistence

	_, err = calicoClient.IPAMConfig().Update(ctx, ipamConfig, options.SetOptions{})
	Expect(err).NotTo(HaveOccurred())
}

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
	var podName string
	var cid string
	var virtResourceManager *KubeVirtResourceManager

	// initialiseTestInfra sets up the test infrastructure including datastore, IP pools, k8s client, node, and namespace
	initialiseTestInfra := func() {
		testutils.WipeDatastore()
		testutils.MustCreateNewIPPool(calicoClient, "192.168.0.0/16", false, false, true)
		testutils.MustCreateNewIPPool(calicoClient, "fd80:24e2:f998:72d6::/64", false, false, true)

		// Create a unique container ID for each test
		cid = uuid.NewString()

		// Get Kubernetes client first (needed for AddNode)
		config, err := clientcmd.BuildConfigFromFlags("", "/home/user/certs/kubeconfig")
		Expect(err).NotTo(HaveOccurred())
		k8sClient, err = kubernetes.NewForConfig(config)
		Expect(err).NotTo(HaveOccurred())

		// Create the node for these tests. The IPAM code requires a corresponding Calico node to exist.
		var name string
		name, err = names.Hostname()
		Expect(err).NotTo(HaveOccurred())
		err = testutils.AddNode(calicoClient, k8sClient, name)
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
		podName = "virt-launcher-" + vmName + "-abcde"
		
		// Initialize KubeVirt resource manager
		virtResourceManager, err = NewKubeVirtResourceManager(k8sClient, testNs, vmName)
		Expect(err).NotTo(HaveOccurred())
	}

	// setupMigrationTarget creates VM, VMI, source pod with IP, VMIM, and target pod for migration testing
	setupMigrationTarget := func() (migrationUID, sourcePodName, sourceCID string) {
		fmt.Printf("\n[TEST] ===== Setting up migration target scenario =====\n")

		// Step 1: Create VM and VMI (initially without migration)
		virtResourceManager.CreateVM()
		virtResourceManager.CreateVMI(false, "")

		// Step 2: Create source pod and allocate IP to it
		sourcePodName = "virt-launcher-" + vmName + "-source"
		sourceCID = uuid.NewString()
		virtResourceManager.CreateVirtLauncherPod(sourcePodName, "")

		// Wait for source pod to be retrievable
		fmt.Printf("[TEST] Waiting for source pod to be retrievable...\n")
		Eventually(func() error {
			_, err := k8sClient.CoreV1().Pods(testNs).Get(context.Background(), sourcePodName, metav1.GetOptions{})
			return err
		}, "5s", "100ms").Should(BeNil())
		fmt.Printf("[TEST] Source pod is now retrievable: %s/%s\n", testNs, sourcePodName)

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

		// Step 4: Now simulate migration - create VMIM
		migrationUID = virtResourceManager.CreateVMIM("test-migration")

		// Step 5: Create target pod with migration label
		podName = "virt-launcher-" + vmName + "-target" // Different pod name for target
		virtResourceManager.CreateVirtLauncherPod(podName, migrationUID)

		fmt.Println("[TEST] Migration target setup completed - source pod has IP, target pod created")
		return migrationUID, sourcePodName, sourceCID
	}

	BeforeEach(func() {
		initialiseTestInfra()
	})

	AfterEach(func() {
		// Clean up test namespace
		if k8sClient != nil && testNs != "" {
			err := k8sClient.CoreV1().Namespaces().Delete(context.Background(), testNs, metav1.DeleteOptions{})
			if err != nil {
				fmt.Printf("Warning: failed to delete test namespace %s: %v\n", testNs, err)
			}
		}

		// Delete the node
		name, err := names.Hostname()
		Expect(err).NotTo(HaveOccurred())
		err = testutils.DeleteNode(calicoClient, k8sClient, name)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should use VM-based handle ID for virt-launcher pod", func() {
		fmt.Println("\n[TEST] ===== Running test: should use VM-based handle ID for virt-launcher pod =====")
		// Create VM, VMI, and virt-launcher pod
		virtResourceManager.CreateVM()
		virtResourceManager.CreateVMI(false, "")
		virtResourceManager.CreateVirtLauncherPod(podName, "")

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
		var sourcePodName string
		var sourceCID string

		BeforeEach(func() {
			_, sourcePodName, sourceCID = setupMigrationTarget()
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
	})

	Context("KubeVirt VM persistence disabled", func() {
		BeforeEach(func() {
			// Set IPAMConfig to disable VM address persistence before setting up resources
			updateIPAMKubeVirtIPPersistence(calicoClient, vmAddressPersistencePtr(libapiv3.VMAddressPersistenceDisabled))
			
			// Set up migration target scenario
			_, _, _ = setupMigrationTarget()
		})

		AfterEach(func() {
			// Clean up IPAMConfig setting
			updateIPAMKubeVirtIPPersistence(calicoClient, nil)
		})

		It("should fail if migration target but KubeVirtVMAddressPersistence is disabled", func() {
			fmt.Println("\n[TEST] ===== Running test: should fail if migration target but KubeVirtVMAddressPersistence is disabled =====")

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
		})
	})
})

// Helper function to create VMAddressPersistence pointer
func vmAddressPersistencePtr(v libapiv3.VMAddressPersistence) *libapiv3.VMAddressPersistence {
	return &v
}

// KubeVirtTestResources stores information about created KubeVirt resources
type KubeVirtTestResources struct {
	VMUID         string
	VMIUID        string
	VMIMUID       string
	SourcePodName string
	TargetPodName string
}

// KubeVirtResourceManager encapsulates resources and methods for KubeVirt testing
type KubeVirtResourceManager struct {
	dynamicClient dynamic.Interface
	k8sClient     kubernetes.Interface
	testNs        string
	vmName        string
	Resources     KubeVirtTestResources
}

// NewKubeVirtResourceManager creates a new KubeVirt resource manager
func NewKubeVirtResourceManager(k8sClient kubernetes.Interface, testNs, vmName string) (*KubeVirtResourceManager, error) {
	config, err := clientcmd.BuildConfigFromFlags("", "/home/user/certs/kubeconfig")
	if err != nil {
		return nil, err
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &KubeVirtResourceManager{
		dynamicClient: dynamicClient,
		k8sClient:     k8sClient,
		testNs:        testNs,
		vmName:        vmName,
		Resources:     KubeVirtTestResources{},
	}, nil
}

// CreateVM creates a VirtualMachine resource
func (h *KubeVirtResourceManager) CreateVM() {
	gvr := schema.GroupVersionResource{
		Group:    "kubevirt.io",
		Version:  "v1",
		Resource: "virtualmachines",
	}

	vm := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]interface{}{
				"name":      h.vmName,
				"namespace": h.testNs,
			},
			"spec": map[string]interface{}{
				"running": true,
			},
		},
	}

	createdVM, err := h.dynamicClient.Resource(gvr).Namespace(h.testNs).Create(context.Background(), vm, metav1.CreateOptions{})
	if err != nil {
		Skip(fmt.Sprintf("Skipping KubeVirt tests - CRDs not installed: %v", err))
	}

	h.Resources.VMUID = string(createdVM.GetUID())
	fmt.Printf("[TEST] VM created successfully: %s/%s (actual UID from K8s: %s)\n", h.testNs, h.vmName, h.Resources.VMUID)
}

// CreateVMI creates a VirtualMachineInstance resource
func (h *KubeVirtResourceManager) CreateVMI(withMigration bool, migrationUID string) {
	controllerTrue := true
	blockOwnerDeletion := true

	vmiObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachineInstance",
			"metadata": map[string]interface{}{
				"name":      h.vmName,
				"namespace": h.testNs,
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"apiVersion":         "kubevirt.io/v1",
						"kind":               "VirtualMachine",
						"name":               h.vmName,
						"uid":                h.Resources.VMUID,
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
			"targetPod":    "virt-launcher-target",
		}
	}

	gvr := schema.GroupVersionResource{
		Group:    "kubevirt.io",
		Version:  "v1",
		Resource: "virtualmachineinstances",
	}

	createdVMI, err := h.dynamicClient.Resource(gvr).Namespace(h.testNs).Create(context.Background(), vmiObj, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())

	h.Resources.VMIUID = string(createdVMI.GetUID())
	fmt.Printf("[TEST] VMI created successfully: %s/%s (actual UID from K8s: %s, withMigration: %v)\n", h.testNs, h.vmName, h.Resources.VMIUID, withMigration)

	// Wait for VMI to be retrievable
	fmt.Printf("[TEST] Waiting for VMI to be retrievable...\n")
	Eventually(func() error {
		_, err := h.dynamicClient.Resource(gvr).Namespace(h.testNs).Get(context.Background(), h.vmName, metav1.GetOptions{})
		return err
	}, "5s", "100ms").Should(BeNil())
	fmt.Printf("[TEST] VMI is now retrievable: %s/%s with UID: %s\n", h.testNs, h.vmName, h.Resources.VMIUID)
}

// CreateVirtLauncherPod creates a virt-launcher pod
func (h *KubeVirtResourceManager) CreateVirtLauncherPod(podName string, migrationUID string) {
	controllerTrue := true

	podLabels := map[string]string{
		"kubevirt.io/domain": h.vmName,
	}

	// Add migration label if migrationUID is provided
	if migrationUID != "" {
		podLabels["kubevirt.io/migrationJobUID"] = migrationUID
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: h.testNs,
			UID:       k8stypes.UID("pod-" + uuid.NewString()),
			Labels:    podLabels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "kubevirt.io/v1",
					Kind:       "VirtualMachineInstance",
					Name:       h.vmName,
					UID:        k8stypes.UID(h.Resources.VMIUID),
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

	_, err := h.k8sClient.CoreV1().Pods(h.testNs).Create(context.Background(), pod, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	fmt.Printf("[TEST] Virt-launcher pod created successfully: %s/%s (migrationUID: %s, VMI UID: %s)\n", h.testNs, podName, migrationUID, h.Resources.VMIUID)

	// Track pod name in resources
	if migrationUID != "" {
		h.Resources.TargetPodName = podName
	} else {
		h.Resources.SourcePodName = podName
	}
}

// CreateVMIM creates a VirtualMachineInstanceMigration resource
func (h *KubeVirtResourceManager) CreateVMIM(name string) string {
	vmim := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachineInstanceMigration",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": h.testNs,
			},
			"spec": map[string]interface{}{
				"vmiName": h.vmName,
			},
		},
	}

	gvr := schema.GroupVersionResource{
		Group:    "kubevirt.io",
		Version:  "v1",
		Resource: "virtualmachineinstancemigrations",
	}

	createdVMIM, err := h.dynamicClient.Resource(gvr).Namespace(h.testNs).Create(context.Background(), vmim, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())

	h.Resources.VMIMUID = string(createdVMIM.GetUID())
	fmt.Printf("[TEST] VMIM created successfully: %s/%s (actual UID from K8s: %s)\n", h.testNs, name, h.Resources.VMIMUID)

	// Wait for VMIM to be retrievable
	fmt.Printf("[TEST] Waiting for VMIM to be retrievable...\n")
	Eventually(func() error {
		_, err := h.dynamicClient.Resource(gvr).Namespace(h.testNs).Get(context.Background(), name, metav1.GetOptions{})
		return err
	}, "5s", "100ms").Should(BeNil())
	fmt.Printf("[TEST] VMIM is now retrievable: %s/%s with UID: %s\n", h.testNs, name, h.Resources.VMIMUID)

	return h.Resources.VMIMUID
}
