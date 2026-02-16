// Copyright (c) 2026 Tigera, Inc. All rights reserved.

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
	"github.com/projectcalico/calico/libcalico-go/lib/errors"
	"github.com/projectcalico/calico/libcalico-go/lib/ipam"
	"github.com/projectcalico/calico/libcalico-go/lib/names"
	cnet "github.com/projectcalico/calico/libcalico-go/lib/net"
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

// GetCNIArgsForPod constructs a CNI_ARGS string matching the format kubelet uses in production.
// Kubelet always prepends IgnoreUnknown=1 so that CNI/IPAM plugins using cnitypes.LoadArgs
// with cnitypes.CommonArgs will ignore fields they don't define (e.g., K8S_POD_NAME).
// Without IgnoreUnknown=1, LoadArgs would fail on any field not present in the plugin's args struct.
func GetCNIArgsForPod(podName, namespace, containerID string) string {
	return fmt.Sprintf("IgnoreUnknown=1;K8S_POD_NAME=%s;K8S_POD_NAMESPACE=%s;K8S_POD_INFRA_CONTAINER_ID=%s",
		podName, namespace, containerID)
}

// verifyOwnerAttributes checks the IPAM owner attributes for all IPs allocated to a VMI handle.
// It verifies:
//   - ActiveOwnerAttrs: pod name, namespace, VMI name, VMI UID, VM UID, migration role = "active"
//   - AlternateOwnerAttrs: if expectAlternate is true, verifies target pod info and migration role = "alternate"
func verifyOwnerAttributes(
	calicoClient client.Interface,
	networkName string,
	namespace string,
	vmName string,
	expectedActivePodName string,
	expectedVMIUID string,
	expectedVMUID string,
	expectedAlternatePodName string,
	expectedVMIMUID string,
) {
	ctx := context.Background()
	handleID := ipam.CreateVMIHandleID(networkName, namespace, vmName)

	ips, err := calicoClient.IPAM().IPsByHandle(ctx, handleID)
	Expect(err).NotTo(HaveOccurred(), "Failed to get IPs by handle %s", handleID)
	Expect(ips).NotTo(BeEmpty(), "No IPs found for handle %s", handleID)

	for _, ip := range ips {
		allocAttr, err := calicoClient.IPAM().GetAssignmentAttributes(ctx, cnet.IP{IP: ip.IP})
		Expect(err).NotTo(HaveOccurred(), "Failed to get assignment attributes for IP %s", ip)
		Expect(allocAttr).NotTo(BeNil(), "No allocation attributes for IP %s", ip)

		fmt.Printf("[TEST] Verifying attributes for IP %s (handle: %s)\n", ip, handleID)

		// Verify ActiveOwnerAttrs
		Expect(allocAttr.ActiveOwnerAttrs).NotTo(BeNil(), "ActiveOwnerAttrs should not be nil")
		Expect(allocAttr.ActiveOwnerAttrs[ipam.AttributePod]).To(Equal(expectedActivePodName),
			"ActiveOwnerAttrs pod name mismatch")
		Expect(allocAttr.ActiveOwnerAttrs[ipam.AttributeNamespace]).To(Equal(namespace),
			"ActiveOwnerAttrs namespace mismatch")
		Expect(allocAttr.ActiveOwnerAttrs[ipam.AttributeVMIName]).To(Equal(vmName),
			"ActiveOwnerAttrs VMI name mismatch")
		Expect(allocAttr.ActiveOwnerAttrs[ipam.AttributeVMIUID]).To(Equal(expectedVMIUID),
			"ActiveOwnerAttrs VMI UID mismatch")
		Expect(allocAttr.ActiveOwnerAttrs[ipam.AttributeMigrationRole]).To(Equal("active"),
			"ActiveOwnerAttrs migration role should be 'active'")
		if expectedVMUID != "" {
			Expect(allocAttr.ActiveOwnerAttrs[ipam.AttributeVMUID]).To(Equal(expectedVMUID),
				"ActiveOwnerAttrs VM UID mismatch")
		}

		// Verify AlternateOwnerAttrs
		if expectedAlternatePodName != "" {
			Expect(allocAttr.AlternateOwnerAttrs).NotTo(BeNil(), "AlternateOwnerAttrs should not be nil for migration target")
			Expect(allocAttr.AlternateOwnerAttrs[ipam.AttributePod]).To(Equal(expectedAlternatePodName),
				"AlternateOwnerAttrs pod name mismatch")
			Expect(allocAttr.AlternateOwnerAttrs[ipam.AttributeNamespace]).To(Equal(namespace),
				"AlternateOwnerAttrs namespace mismatch")
			Expect(allocAttr.AlternateOwnerAttrs[ipam.AttributeVMIName]).To(Equal(vmName),
				"AlternateOwnerAttrs VMI name mismatch")
			Expect(allocAttr.AlternateOwnerAttrs[ipam.AttributeVMIUID]).To(Equal(expectedVMIUID),
				"AlternateOwnerAttrs VMI UID mismatch")
			Expect(allocAttr.AlternateOwnerAttrs[ipam.AttributeMigrationRole]).To(Equal("alternate"),
				"AlternateOwnerAttrs migration role should be 'alternate'")
			if expectedVMIMUID != "" {
				Expect(allocAttr.AlternateOwnerAttrs[ipam.AttributeVMIMUID]).To(Equal(expectedVMIMUID),
					"AlternateOwnerAttrs VMIM UID mismatch")
			}
			if expectedVMUID != "" {
				Expect(allocAttr.AlternateOwnerAttrs[ipam.AttributeVMUID]).To(Equal(expectedVMUID),
					"AlternateOwnerAttrs VM UID mismatch")
			}
		} else {
			// No migration target expected - AlternateOwnerAttrs should be nil or empty
			if allocAttr.AlternateOwnerAttrs != nil {
				Expect(allocAttr.AlternateOwnerAttrs).To(BeEmpty(),
					"AlternateOwnerAttrs should be empty when no migration target is expected")
			}
		}

		fmt.Printf("[TEST] Owner attributes verified for IP %s\n", ip)
	}
}

// getDualStackNetconf returns a netconf JSON string that requests both IPv4 and IPv6 addresses.
func getDualStackNetconf(cniVersion string) string {
	return fmt.Sprintf(`{
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
			"type": "calico-ipam",
			"assign_ipv4": "true",
			"assign_ipv6": "true"
		}
	}`, cniVersion, os.Getenv("ETCD_IP"))
}

// updateIPAMKubeVirtIPPersistence updates the KubeVirtVMAddressPersistence setting in IPAMConfig
func updateIPAMKubeVirtIPPersistence(calicoClient client.Interface, persistence *libapiv3.VMAddressPersistence) {
	ctx := context.Background()
	ipamConfig, err := calicoClient.IPAMConfig().Get(ctx, "default", options.GetOptions{})

	if err != nil {
		// Check if error is specifically because the resource doesn't exist
		if _, ok := err.(errors.ErrorResourceDoesNotExist); !ok {
			// It's a different error, fail the test
			Expect(err).NotTo(HaveOccurred())
		}

		// IPAMConfig doesn't exist, create it
		fmt.Printf("[TEST] Creating IPAMConfig with KubeVirtVMAddressPersistence = %v\n", persistence)
		ipamConfig = &libapiv3.IPAMConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: "default",
			},
			Spec: libapiv3.IPAMConfigSpec{
				StrictAffinity:               false,
				AutoAllocateBlocks:           true,
				MaxBlocksPerHost:             0,
				KubeVirtVMAddressPersistence: persistence,
			},
		}
		_, err = calicoClient.IPAMConfig().Create(ctx, ipamConfig, options.SetOptions{})
		Expect(err).NotTo(HaveOccurred())
		fmt.Printf("[TEST] IPAMConfig created successfully\n")
	} else {
		// IPAMConfig exists, update it
		fmt.Printf("[TEST] Updating existing IPAMConfig with KubeVirtVMAddressPersistence = %v\n", persistence)
		ipamConfig.Spec.KubeVirtVMAddressPersistence = persistence
		_, err = calicoClient.IPAMConfig().Update(ctx, ipamConfig, options.SetOptions{})
		Expect(err).NotTo(HaveOccurred())
		fmt.Printf("[TEST] IPAMConfig updated successfully\n")
	}
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

		// Step 3: Allocate dual-stack IPs to source pod
		fmt.Printf("[TEST] Allocating dual-stack IPs to source pod %s\n", sourcePodName)
		netconf := getDualStackNetconf(cniVersion)

		sourceCNIArgs := GetCNIArgsForPod(sourcePodName, testNs, sourceCID)
		result, errOut, exitCode := testutils.RunIPAMPlugin(netconf, "ADD", sourceCNIArgs, sourceCID, cniVersion)
		Expect(exitCode).To(Equal(0), fmt.Sprintf("Source pod IPAM ADD failed: %v", errOut))
		Expect(result.IPs).To(HaveLen(2), "Expected dual-stack: one IPv4 and one IPv6")
		fmt.Printf("[TEST] Source pod allocated IPs: %s, %s\n", result.IPs[0].Address.IP.String(), result.IPs[1].Address.IP.String())

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

		netconf := getDualStackNetconf(cniVersion)
		cniArgs := GetCNIArgsForPod(podName, testNs, cid)

		// Run IPAM ADD - should get one IPv4 and one IPv6
		result, errOut, exitCode := testutils.RunIPAMPlugin(netconf, "ADD", cniArgs, cid, cniVersion)
		Expect(exitCode).To(Equal(0), fmt.Sprintf("IPAM ADD failed: %v", errOut))
		Expect(result.IPs).To(HaveLen(2), "Expected dual-stack: one IPv4 and one IPv6")

		// Verify one IPv4 and one IPv6 address
		var hasIPv4, hasIPv6 bool
		for _, ipConfig := range result.IPs {
			if ipConfig.Address.IP.To4() != nil {
				hasIPv4 = true
			} else {
				hasIPv6 = true
			}
		}
		Expect(hasIPv4).To(BeTrue(), "Expected an IPv4 address in result")
		Expect(hasIPv6).To(BeTrue(), "Expected an IPv6 address in result")

		// Verify routes are populated for normal pod
		verifyRoutesPopulatedInResult(result, true)

		// Verify owner attributes: source pod should be ActiveOwner, no AlternateOwner
		verifyOwnerAttributes(
			calicoClient,
			"net1",      // network name from netconf
			testNs,      // namespace
			vmName,      // VMI name (same as VM name)
			podName,     // expected active pod name
			virtResourceManager.Resources.VMIUID,
			virtResourceManager.Resources.VMUID,
			"", // no alternate pod (not a migration)
			"", // no VMIM UID
		)

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
				netconf := getDualStackNetconf(cniVersion)
				sourceCNIArgs := GetCNIArgsForPod(sourcePodName, testNs, sourceCID)
				testutils.RunIPAMPlugin(netconf, "DEL", sourceCNIArgs, sourceCID, cniVersion)
			}
		})

		It("should return empty routes for migration target pod", func() {
			fmt.Println("\n[TEST] ===== Running test: should return empty routes for migration target pod =====")
			netconf := getDualStackNetconf(cniVersion)

			// Verify source pod is ActiveOwner before migration target ADD
			verifyOwnerAttributes(
				calicoClient,
				"net1",
				testNs,
				vmName,
				sourcePodName, // source pod is active owner
				virtResourceManager.Resources.VMIUID,
				virtResourceManager.Resources.VMUID,
				"", // no alternate yet
				"",
			)

			cniArgs := GetCNIArgsForPod(podName, testNs, cid)

			// Run IPAM ADD for migration target - should get both existing IPs (IPv4 + IPv6)
			result, errOut, exitCode := testutils.RunIPAMPlugin(netconf, "ADD", cniArgs, cid, cniVersion)
			Expect(exitCode).To(Equal(0), fmt.Sprintf("IPAM ADD failed: %v", errOut))
			Expect(result.IPs).To(HaveLen(2), "Expected dual-stack: migration target should receive both existing IPs")

			// Verify empty routes for migration target
			verifyRoutesPopulatedInResult(result, false)

			// Verify owner attributes after migration target ADD:
			// - ActiveOwnerAttrs should still be the source pod
			// - AlternateOwnerAttrs should now be the target pod
			verifyOwnerAttributes(
				calicoClient,
				"net1",
				testNs,
				vmName,
				sourcePodName, // source pod remains active owner
				virtResourceManager.Resources.VMIUID,
				virtResourceManager.Resources.VMUID,
				podName, // target pod is now alternate owner
				virtResourceManager.Resources.VMIMUID,
			)

			// Clean up
			_, _, exitCode = testutils.RunIPAMPlugin(netconf, "DEL", cniArgs, cid, cniVersion)
			Expect(exitCode).To(Equal(0))
		})
	})

	Context("KubeVirt VM persistence disabled", func() {
		BeforeEach(func() {
			// Set IPAMConfig to disable VM address persistence before setting up resources
			// (Parent BeforeEach already called initialiseTestInfra which wiped datastore)
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

			netconf := getDualStackNetconf(cniVersion)
			cniArgs := GetCNIArgsForPod(podName, testNs, cid)

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
