// Copyright (c) 2026 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package node

import (
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/projectcalico/calico/libcalico-go/lib/ipam"
	"github.com/projectcalico/calico/libcalico-go/lib/kubevirt"
)

var _ = Describe("VMI IPAM GC UTs", func() {
	Describe("allocation helper methods", func() {
		It("should correctly identify VMI allocations with isVMIIP()", func() {
			// Allocation with VMI attribute
			vmiAlloc := &allocation{
				ip:     "10.0.0.1",
				handle: "test-handle",
				attrs: map[string]string{
					ipam.AttributeVMI:       "test-vmi",
					ipam.AttributeNamespace: "test-ns",
				},
			}
			Expect(vmiAlloc.isVMIIP()).To(BeTrue())

			// Allocation without VMI attribute (regular pod)
			podAlloc := &allocation{
				ip:     "10.0.0.2",
				handle: "test-handle",
				attrs: map[string]string{
					ipam.AttributePod:       "test-pod",
					ipam.AttributeNamespace: "test-ns",
				},
			}
			Expect(podAlloc.isVMIIP()).To(BeFalse())

			// Empty allocation
			emptyAlloc := &allocation{
				ip:     "10.0.0.3",
				handle: "test-handle",
				attrs:  map[string]string{},
			}
			Expect(emptyAlloc.isVMIIP()).To(BeFalse())
		})

		It("should correctly return VMI name with getVMIName()", func() {
			// Allocation with VMI attribute
			vmiAlloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeVMI: "my-vmi",
				},
			}
			Expect(vmiAlloc.getVMIName()).To(Equal("my-vmi"))

			// Allocation without VMI attribute
			noVMIAlloc := &allocation{
				attrs: map[string]string{
					ipam.AttributePod: "my-pod",
				},
			}
			Expect(noVMIAlloc.getVMIName()).To(Equal(""))
		})
	})

	Describe("getVMOwnerRef", func() {
		It("should return the VirtualMachine owner reference", func() {
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "test-ns",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "kubevirt.io/v1",
							Kind:       "VirtualMachine",
							Name:       "test-vm",
							UID:        "vm-uid-123",
						},
					},
				},
			}
			ref := getVMOwnerRef(vmi)
			Expect(ref).NotTo(BeNil())
			Expect(ref.Name).To(Equal("test-vm"))
			Expect(ref.Kind).To(Equal("VirtualMachine"))
			Expect(ref.UID).To(Equal(types.UID("vm-uid-123")))
		})

		It("should return nil if no VirtualMachine owner reference exists", func() {
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "test-ns",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "ReplicaSet",
							Name:       "some-rs",
						},
					},
				},
			}
			ref := getVMOwnerRef(vmi)
			Expect(ref).To(BeNil())
		})

		It("should return nil if owner references is empty", func() {
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-vmi",
					Namespace:       "test-ns",
					OwnerReferences: []metav1.OwnerReference{},
				},
			}
			ref := getVMOwnerRef(vmi)
			Expect(ref).To(BeNil())
		})

		It("should ignore non-kubevirt.io VirtualMachine references", func() {
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "test-ns",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "other.io/v1",
							Kind:       "VirtualMachine",
							Name:       "other-vm",
						},
					},
				},
			}
			ref := getVMOwnerRef(vmi)
			Expect(ref).To(BeNil())
		})
	})

	Describe("withinGracePeriod", func() {
		var logEntry *log.Entry

		BeforeEach(func() {
			logEntry = log.WithFields(log.Fields{"test": "withinGracePeriod"})
		})

		It("should return true if leakedAt is nil (first detection)", func() {
			alloc := &allocation{
				leakedAt: nil,
			}
			result := withinGracePeriod(alloc, logEntry)
			Expect(result).To(BeTrue())
		})

		It("should return true if within the grace period", func() {
			recentTime := time.Now().Add(-1 * time.Minute) // 1 minute ago
			alloc := &allocation{
				leakedAt: &recentTime,
			}
			result := withinGracePeriod(alloc, logEntry)
			Expect(result).To(BeTrue())
		})

		It("should return false if beyond the grace period", func() {
			oldTime := time.Now().Add(-10 * time.Minute) // 10 minutes ago (> 5 min grace)
			alloc := &allocation{
				leakedAt: &oldTime,
			}
			result := withinGracePeriod(alloc, logEntry)
			Expect(result).To(BeFalse())
		})
	})

	Describe("vmAllowsIP", func() {
		var logEntry *log.Entry

		BeforeEach(func() {
			logEntry = log.WithFields(log.Fields{"test": "vmAllowsIP"})
		})

		It("should return false if VM is being deleted", func() {
			now := metav1.Now()
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-vm",
					Namespace:         "test-ns",
					DeletionTimestamp: &now,
				},
			}
			result := vmAllowsIP(vm, logEntry)
			Expect(result).To(BeFalse())
		})

		It("should return true if VM is not being deleted and run strategy is not Halted", func() {
			running := true
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vm",
					Namespace: "test-ns",
				},
				Spec: kubevirtv1.VirtualMachineSpec{
					Running: &running,
				},
			}
			result := vmAllowsIP(vm, logEntry)
			Expect(result).To(BeTrue())
		})

		It("should return false if VM RunStrategy is Halted", func() {
			runStrategy := kubevirtv1.RunStrategyHalted
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vm",
					Namespace: "test-ns",
				},
				Spec: kubevirtv1.VirtualMachineSpec{
					RunStrategy: &runStrategy,
				},
			}
			result := vmAllowsIP(vm, logEntry)
			Expect(result).To(BeFalse())
		})

		It("should return true if VM RunStrategy is Always", func() {
			runStrategy := kubevirtv1.RunStrategyAlways
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vm",
					Namespace: "test-ns",
				},
				Spec: kubevirtv1.VirtualMachineSpec{
					RunStrategy: &runStrategy,
				},
			}
			result := vmAllowsIP(vm, logEntry)
			Expect(result).To(BeTrue())
		})
	})

	Describe("IPAMController.isMigrating", func() {
		var fakeVirtClient *kubevirt.FakeVirtClient
		var c *IPAMController

		BeforeEach(func() {
			fakeVirtClient = kubevirt.NewFakeVirtClient()
			c = &IPAMController{
				virtClient: fakeVirtClient,
			}
		})

		It("should return false if virtClient is nil", func() {
			c.virtClient = nil
			result := c.isMigrating("test-ns", "test-vmi")
			Expect(result).To(BeFalse())
		})

		It("should return false if no migrations exist", func() {
			result := c.isMigrating("test-ns", "test-vmi")
			Expect(result).To(BeFalse())
		})

		It("should return true if an active migration exists for the VMI", func() {
			migration := &kubevirtv1.VirtualMachineInstanceMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migration",
					Namespace: "test-ns",
				},
				Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
					VMIName: "test-vmi",
				},
				Status: kubevirtv1.VirtualMachineInstanceMigrationStatus{
					Phase: kubevirtv1.MigrationRunning,
				},
			}
			fakeVirtClient.AddMigration(migration)

			result := c.isMigrating("test-ns", "test-vmi")
			Expect(result).To(BeTrue())
		})

		It("should return false if migration has succeeded", func() {
			migration := &kubevirtv1.VirtualMachineInstanceMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migration",
					Namespace: "test-ns",
				},
				Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
					VMIName: "test-vmi",
				},
				Status: kubevirtv1.VirtualMachineInstanceMigrationStatus{
					Phase: kubevirtv1.MigrationSucceeded,
				},
			}
			fakeVirtClient.AddMigration(migration)

			result := c.isMigrating("test-ns", "test-vmi")
			Expect(result).To(BeFalse())
		})

		It("should return false if migration has failed", func() {
			migration := &kubevirtv1.VirtualMachineInstanceMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migration",
					Namespace: "test-ns",
				},
				Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
					VMIName: "test-vmi",
				},
				Status: kubevirtv1.VirtualMachineInstanceMigrationStatus{
					Phase: kubevirtv1.MigrationFailed,
				},
			}
			fakeVirtClient.AddMigration(migration)

			result := c.isMigrating("test-ns", "test-vmi")
			Expect(result).To(BeFalse())
		})

		It("should return false if migration is for a different VMI", func() {
			migration := &kubevirtv1.VirtualMachineInstanceMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migration",
					Namespace: "test-ns",
				},
				Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
					VMIName: "other-vmi",
				},
				Status: kubevirtv1.VirtualMachineInstanceMigrationStatus{
					Phase: kubevirtv1.MigrationRunning,
				},
			}
			fakeVirtClient.AddMigration(migration)

			result := c.isMigrating("test-ns", "test-vmi")
			Expect(result).To(BeFalse())
		})
	})

	Describe("IPAMController.getVmiByNameAndGuid", func() {
		var fakeVirtClient *kubevirt.FakeVirtClient
		var c *IPAMController

		BeforeEach(func() {
			fakeVirtClient = kubevirt.NewFakeVirtClient()
			c = &IPAMController{
				virtClient: fakeVirtClient,
			}
		})

		It("should return VMI if UID matches", func() {
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "test-ns",
					UID:       "expected-uid-123",
				},
			}
			fakeVirtClient.AddVMI(vmi)

			result, err := c.getVmiByNameAndGuid("test-ns", "test-vmi", "expected-uid-123")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.Name).To(Equal("test-vmi"))
		})

		It("should return nil if UID does not match", func() {
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "test-ns",
					UID:       "different-uid",
				},
			}
			fakeVirtClient.AddVMI(vmi)

			result, err := c.getVmiByNameAndGuid("test-ns", "test-vmi", "expected-uid-123")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())
		})

		It("should return nil if VMI is being deleted", func() {
			now := metav1.Now()
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-vmi",
					Namespace:         "test-ns",
					UID:               "expected-uid-123",
					DeletionTimestamp: &now,
				},
			}
			fakeVirtClient.AddVMI(vmi)

			result, err := c.getVmiByNameAndGuid("test-ns", "test-vmi", "expected-uid-123")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeNil())
		})

		It("should return error if VMI does not exist", func() {
			result, err := c.getVmiByNameAndGuid("test-ns", "nonexistent-vmi", "some-uid")
			Expect(err).To(HaveOccurred())
			Expect(result).To(BeNil())
		})
	})

	Describe("IPAMController.vmiAllocationIsValid", func() {
		var fakeVirtClient *kubevirt.FakeVirtClient
		var c *IPAMController

		BeforeEach(func() {
			fakeVirtClient = kubevirt.NewFakeVirtClient()
			c = &IPAMController{
				virtClient: fakeVirtClient,
			}
		})

		It("should return true if virtClient is nil", func() {
			c.virtClient = nil
			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeVMI:       "test-vmi",
					ipam.AttributeNamespace: "test-ns",
					"vmiuid":                "vmi-uid-123",
				},
			}
			result := c.vmiAllocationIsValid(alloc, false)
			Expect(result).To(BeTrue())
		})

		It("should return true if namespace is empty", func() {
			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeVMI: "test-vmi",
					"vmiuid":          "vmi-uid-123",
				},
			}
			result := c.vmiAllocationIsValid(alloc, false)
			Expect(result).To(BeTrue())
		})

		It("should return true if VMI name is empty", func() {
			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeNamespace: "test-ns",
					"vmiuid":                "vmi-uid-123",
				},
			}
			result := c.vmiAllocationIsValid(alloc, false)
			Expect(result).To(BeTrue())
		})

		It("should return true if VMI UID is empty", func() {
			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeVMI:       "test-vmi",
					ipam.AttributeNamespace: "test-ns",
				},
			}
			result := c.vmiAllocationIsValid(alloc, false)
			Expect(result).To(BeTrue())
		})

		It("should return true if VMI exists with matching UID and VM allows IP", func() {
			// Create a VM
			runStrategy := kubevirtv1.RunStrategyAlways
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vm",
					Namespace: "test-ns",
					UID:       "vm-uid-123",
				},
				Spec: kubevirtv1.VirtualMachineSpec{
					RunStrategy: &runStrategy,
				},
			}
			fakeVirtClient.AddVM(vm)

			// Create a VMI owned by the VM
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "test-ns",
					UID:       "vmi-uid-123",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "kubevirt.io/v1",
							Kind:       "VirtualMachine",
							Name:       "test-vm",
							UID:        "vm-uid-123",
						},
					},
				},
			}
			fakeVirtClient.AddVMI(vmi)

			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeVMI:       "test-vmi",
					ipam.AttributeNamespace: "test-ns",
					"vmiuid":                "vmi-uid-123",
				},
			}

			result := c.vmiAllocationIsValid(alloc, false)
			Expect(result).To(BeTrue())
		})

		It("should return false if VM is being deleted", func() {
			// Create a VM that's being deleted
			now := metav1.Now()
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-vm",
					Namespace:         "test-ns",
					UID:               "vm-uid-123",
					DeletionTimestamp: &now,
				},
			}
			fakeVirtClient.AddVM(vm)

			// Create a VMI owned by the VM
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "test-ns",
					UID:       "vmi-uid-123",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "kubevirt.io/v1",
							Kind:       "VirtualMachine",
							Name:       "test-vm",
							UID:        "vm-uid-123",
						},
					},
				},
			}
			fakeVirtClient.AddVMI(vmi)

			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeVMI:       "test-vmi",
					ipam.AttributeNamespace: "test-ns",
					"vmiuid":                "vmi-uid-123",
				},
			}

			result := c.vmiAllocationIsValid(alloc, false)
			Expect(result).To(BeFalse())
		})

		It("should return true during active migration even if VMI missing", func() {
			// Create a VM that allows IP
			runStrategy := kubevirtv1.RunStrategyAlways
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vm",
					Namespace: "test-ns",
					UID:       "vm-uid-123",
				},
				Spec: kubevirtv1.VirtualMachineSpec{
					RunStrategy: &runStrategy,
				},
			}
			fakeVirtClient.AddVM(vm)

			// Create an active migration (no VMI needed)
			migration := &kubevirtv1.VirtualMachineInstanceMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migration",
					Namespace: "test-ns",
				},
				Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
					VMIName: "test-vmi",
				},
				Status: kubevirtv1.VirtualMachineInstanceMigrationStatus{
					Phase: kubevirtv1.MigrationRunning,
				},
			}
			fakeVirtClient.AddMigration(migration)

			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeVMI:       "test-vmi",
					ipam.AttributeNamespace: "test-ns",
					ipam.AttributeVM:        "test-vm",
					ipam.AttributeVMUID:     "vm-uid-123",
					"vmiuid":                "old-vmi-uid", // VMI UID that no longer exists
				},
			}

			result := c.vmiAllocationIsValid(alloc, false)
			Expect(result).To(BeTrue())
		})

		It("should return true during grace period when VMI missing but VM allows IP", func() {
			// Create a VM that allows IP
			runStrategy := kubevirtv1.RunStrategyAlways
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vm",
					Namespace: "test-ns",
					UID:       "vm-uid-123",
				},
				Spec: kubevirtv1.VirtualMachineSpec{
					RunStrategy: &runStrategy,
				},
			}
			fakeVirtClient.AddVM(vm)

			// Allocation with no leakedAt (first detection, within grace)
			alloc := &allocation{
				leakedAt: nil, // First time seeing this as a potential leak
				attrs: map[string]string{
					ipam.AttributeVMI:       "test-vmi",
					ipam.AttributeNamespace: "test-ns",
					ipam.AttributeVM:        "test-vm",
					ipam.AttributeVMUID:     "vm-uid-123",
					"vmiuid":                "old-vmi-uid", // VMI UID that no longer exists
				},
			}

			result := c.vmiAllocationIsValid(alloc, false)
			Expect(result).To(BeTrue())
		})

		It("should return false when beyond grace period and VMI missing", func() {
			// Create a VM that allows IP
			runStrategy := kubevirtv1.RunStrategyAlways
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vm",
					Namespace: "test-ns",
					UID:       "vm-uid-123",
				},
				Spec: kubevirtv1.VirtualMachineSpec{
					RunStrategy: &runStrategy,
				},
			}
			fakeVirtClient.AddVM(vm)

			// Allocation with old leakedAt (beyond grace period)
			oldTime := time.Now().Add(-10 * time.Minute)
			alloc := &allocation{
				leakedAt: &oldTime,
				attrs: map[string]string{
					ipam.AttributeVMI:       "test-vmi",
					ipam.AttributeNamespace: "test-ns",
					ipam.AttributeVM:        "test-vm",
					ipam.AttributeVMUID:     "vm-uid-123",
					"vmiuid":                "old-vmi-uid", // VMI UID that no longer exists
				},
			}

			result := c.vmiAllocationIsValid(alloc, false)
			Expect(result).To(BeFalse())
		})
	})

	Describe("IPAMController.resolveVMForAllocation", func() {
		var fakeVirtClient *kubevirt.FakeVirtClient
		var c *IPAMController
		var logEntry *log.Entry

		BeforeEach(func() {
			fakeVirtClient = kubevirt.NewFakeVirtClient()
			c = &IPAMController{
				virtClient: fakeVirtClient,
			}
			logEntry = log.WithFields(log.Fields{"test": "resolveVMForAllocation"})
		})

		It("should resolve VM from VMI owner reference", func() {
			// Create VM
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vm",
					Namespace: "test-ns",
					UID:       "vm-uid-123",
				},
			}
			fakeVirtClient.AddVM(vm)

			// Create VMI with owner reference to VM
			vmi := &kubevirtv1.VirtualMachineInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vmi",
					Namespace: "test-ns",
					UID:       "vmi-uid-456",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "kubevirt.io/v1",
							Kind:       "VirtualMachine",
							Name:       "test-vm",
							UID:        "vm-uid-123",
						},
					},
				},
			}
			fakeVirtClient.AddVMI(vmi)

			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeNamespace: "test-ns",
				},
			}

			result := c.resolveVMForAllocation("test-ns", "test-vmi", alloc, logEntry)
			Expect(result).NotTo(BeNil())
			Expect(result.Name).To(Equal("test-vm"))
		})

		It("should resolve VM from stored attributes when VMI missing", func() {
			// Create VM
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vm",
					Namespace: "test-ns",
					UID:       "vm-uid-123",
				},
			}
			fakeVirtClient.AddVM(vm)

			// No VMI in the fake client

			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeNamespace: "test-ns",
					ipam.AttributeVM:        "test-vm",
					ipam.AttributeVMUID:     "vm-uid-123",
				},
			}

			result := c.resolveVMForAllocation("test-ns", "test-vmi", alloc, logEntry)
			Expect(result).NotTo(BeNil())
			Expect(result.Name).To(Equal("test-vm"))
		})

		It("should return nil if stored VM UID doesn't match", func() {
			// Create VM with different UID
			vm := &kubevirtv1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vm",
					Namespace: "test-ns",
					UID:       "new-vm-uid",
				},
			}
			fakeVirtClient.AddVM(vm)

			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeNamespace: "test-ns",
					ipam.AttributeVM:        "test-vm",
					ipam.AttributeVMUID:     "old-vm-uid", // Different from actual VM
				},
			}

			result := c.resolveVMForAllocation("test-ns", "test-vmi", alloc, logEntry)
			Expect(result).To(BeNil())
		})

		It("should return nil if neither VMI nor stored attributes can resolve VM", func() {
			alloc := &allocation{
				attrs: map[string]string{
					ipam.AttributeNamespace: "test-ns",
				},
			}

			result := c.resolveVMForAllocation("test-ns", "test-vmi", alloc, logEntry)
			Expect(result).To(BeNil())
		})
	})
})
