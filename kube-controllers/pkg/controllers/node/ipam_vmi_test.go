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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/projectcalico/calico/libcalico-go/lib/ipam"
	"github.com/projectcalico/calico/libcalico-go/lib/kubevirt"
)

var _ = Describe("vmiAllocationIsValid tests", func() {
	var c *IPAMController
	var fakeVirtClient *kubevirt.FakeVirtClient

	// Helper function to create a basic allocation with VMI attributes
	createVMIAllocation := func(ns, vmiName, vmiUID string) *allocation {
		return &allocation{
			ip:     "10.0.0.1",
			handle: "test-handle",
			attrs: map[string]string{
				ipam.AttributeNamespace: ns,
				ipam.AttributeVMI:       vmiName,
				"vmiuid":                vmiUID,
			},
			block: "10.0.0.0/24",
			knode: "test-node",
		}
	}

	// Helper function to create a VMI
	createVMI := func(name, namespace, uid string) *kubevirtv1.VirtualMachineInstance {
		return &kubevirtv1.VirtualMachineInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				UID:       types.UID(uid),
			},
		}
	}

	// Helper function to create a VM
	createVM := func(name, namespace, uid string) *kubevirtv1.VirtualMachine {
		return &kubevirtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				UID:       types.UID(uid),
			},
			Spec: kubevirtv1.VirtualMachineSpec{
				Running: boolPtr(true),
			},
		}
	}

	// Helper function to create a VMI with owner reference
	createVMIWithOwner := func(name, namespace, uid, vmName, vmUID string) *kubevirtv1.VirtualMachineInstance {
		vmi := createVMI(name, namespace, uid)
		vmi.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: "kubevirt.io/v1",
				Kind:       "VirtualMachine",
				Name:       vmName,
				UID:        types.UID(vmUID),
			},
		}
		return vmi
	}

	BeforeEach(func() {
		fakeVirtClient = kubevirt.NewFakeVirtClient()
		c = &IPAMController{
			virtClient: fakeVirtClient,
		}
	})

	Context("when allocation data is insufficient", func() {
		It("should return true when virtClient is nil", func() {
			c.virtClient = nil
			a := createVMIAllocation("test-ns", "test-vmi", "test-uid")
			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})

		It("should return true when namespace is empty", func() {
			a := createVMIAllocation("", "test-vmi", "test-uid")
			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})

		It("should return true when vmiName is empty", func() {
			a := createVMIAllocation("test-ns", "", "test-uid")
			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})

		It("should return true when vmiUID is empty", func() {
			a := createVMIAllocation("test-ns", "test-vmi", "")
			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})
	})

	Context("when VM cannot be resolved", func() {
		It("should return true within grace period on first check", func() {
			a := createVMIAllocation("test-ns", "test-vmi", "test-uid")
			// First check - starts grace period
			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})

		It("should return false after grace period when VM cannot be resolved", func() {
			a := createVMIAllocation("test-ns", "test-vmi", "test-uid")
			// Simulate that the allocation has been marked as leaked for longer than the grace period
			leakedAt := time.Now().Add(-VMI_RECREATION_GRACE_PERIOD - time.Minute)
			a.leakedAt = &leakedAt
			Expect(c.vmiAllocationIsValid(a, false)).To(BeFalse())
		})

		It("should return true when migrating, even if VM cannot be resolved", func() {
			a := createVMIAllocation("test-ns", "test-vmi", "test-uid")
			leakedAt := time.Now().Add(-VMI_RECREATION_GRACE_PERIOD - time.Minute)
			a.leakedAt = &leakedAt

			// Add an active migration
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

			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})
	})

	Context("when VM is resolved but doesn't allow IP", func() {
		It("should return false when VM is being deleted", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := vmiName
			vmUID := "vm-uid"

			// Create VMI with owner reference
			vmi := createVMIWithOwner(vmiName, ns, vmiUID, vmName, vmUID)
			fakeVirtClient.AddVMI(vmi)

			// Create VM with deletion timestamp
			vm := createVM(vmName, ns, vmUID)
			now := metav1.Now()
			vm.DeletionTimestamp = &now
			fakeVirtClient.AddVM(vm)

			a := createVMIAllocation(ns, vmiName, vmiUID)
			Expect(c.vmiAllocationIsValid(a, false)).To(BeFalse())
		})

		It("should return false when VM RunStrategy is Halted", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := vmiName
			vmUID := "vm-uid"

			// Create VMI with owner reference
			vmi := createVMIWithOwner(vmiName, ns, vmiUID, vmName, vmUID)
			fakeVirtClient.AddVMI(vmi)

			// Create VM with RunStrategy Halted
			vm := createVM(vmName, ns, vmUID)
			runStrategyHalted := kubevirtv1.RunStrategyHalted
			vm.Spec.Running = nil
			vm.Spec.RunStrategy = &runStrategyHalted
			fakeVirtClient.AddVM(vm)

			a := createVMIAllocation(ns, vmiName, vmiUID)
			Expect(c.vmiAllocationIsValid(a, false)).To(BeFalse())
		})
	})

	Context("when VMI exists with matching UID", func() {
		It("should return true when VMI exists with matching UID", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := vmiName
			vmUID := "vm-uid"

			// Create VMI with owner reference
			vmi := createVMIWithOwner(vmiName, ns, vmiUID, vmName, vmUID)
			fakeVirtClient.AddVMI(vmi)

			// Create VM
			vm := createVM(vmName, ns, vmUID)
			fakeVirtClient.AddVM(vm)

			a := createVMIAllocation(ns, vmiName, vmiUID)
			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})

		It("should return true when VMI is being deleted but within grace period", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := vmiName
			vmUID := "vm-uid"

			// Create VMI with owner reference and deletion timestamp
			vmi := createVMIWithOwner(vmiName, ns, vmiUID, vmName, vmUID)
			now := metav1.Now()
			vmi.DeletionTimestamp = &now
			fakeVirtClient.AddVMI(vmi)

			// Create VM
			vm := createVM(vmName, ns, vmUID)
			fakeVirtClient.AddVM(vm)

			a := createVMIAllocation(ns, vmiName, vmiUID)
			// First check starts grace period
			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})
	})

	Context("when VMI does not exist or has different UID", func() {
		It("should return true within grace period when VMI doesn't exist", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := vmiName
			vmUID := "vm-uid"

			// No VMI, but create VM with stored attributes
			a := createVMIAllocation(ns, vmiName, vmiUID)
			a.attrs[ipam.AttributeVM] = vmName
			a.attrs[ipam.AttributeVMUID] = vmUID

			vm := createVM(vmName, ns, vmUID)
			fakeVirtClient.AddVM(vm)

			// First check - starts grace period
			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})

		It("should return false after grace period when VMI doesn't exist", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := vmiName
			vmUID := "vm-uid"
			differentVmiUID := "different-uid"

			// Create VMI with a different UID (simulating VMI recreation with new UID)
			// This makes getVmiByNameAndGuid return (nil, nil) instead of error
			vmi := createVMIWithOwner(vmiName, ns, differentVmiUID, vmName, vmUID)
			fakeVirtClient.AddVMI(vmi)

			// Create VM with stored attributes
			a := createVMIAllocation(ns, vmiName, vmiUID)
			a.attrs[ipam.AttributeVM] = vmName
			a.attrs[ipam.AttributeVMUID] = vmUID

			vm := createVM(vmName, ns, vmUID)
			fakeVirtClient.AddVM(vm)

			// Set leakedAt to beyond grace period
			leakedAt := time.Now().Add(-VMI_RECREATION_GRACE_PERIOD - time.Minute)
			a.leakedAt = &leakedAt

			Expect(c.vmiAllocationIsValid(a, false)).To(BeFalse())
		})

		It("should return true when migrating even if VMI UID doesn't match", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := vmiName
			vmUID := "vm-uid"
			differentVmiUID := "different-uid"

			// Create VMI with a different UID
			vmi := createVMIWithOwner(vmiName, ns, differentVmiUID, vmName, vmUID)
			fakeVirtClient.AddVMI(vmi)

			// Create VM with stored attributes
			a := createVMIAllocation(ns, vmiName, vmiUID)
			a.attrs[ipam.AttributeVM] = vmName
			a.attrs[ipam.AttributeVMUID] = vmUID

			vm := createVM(vmName, ns, vmUID)
			fakeVirtClient.AddVM(vm)

			// Set leakedAt to beyond grace period
			leakedAt := time.Now().Add(-VMI_RECREATION_GRACE_PERIOD - time.Minute)
			a.leakedAt = &leakedAt

			// Add an active migration
			migration := &kubevirtv1.VirtualMachineInstanceMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migration",
					Namespace: ns,
				},
				Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
					VMIName: vmiName,
				},
				Status: kubevirtv1.VirtualMachineInstanceMigrationStatus{
					Phase: kubevirtv1.MigrationRunning,
				},
			}
			fakeVirtClient.AddMigration(migration)

			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})

		It("should return false when VMI exists with different UID and beyond grace period", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "old-uid"
			newVmiUID := "new-uid"
			vmName := vmiName
			vmUID := "vm-uid"

			// Create VMI with new UID (different from allocation's vmiUID)
			vmi := createVMIWithOwner(vmiName, ns, newVmiUID, vmName, vmUID)
			fakeVirtClient.AddVMI(vmi)

			// Create VM
			vm := createVM(vmName, ns, vmUID)
			fakeVirtClient.AddVM(vm)

			a := createVMIAllocation(ns, vmiName, vmiUID)
			// Set leakedAt to beyond grace period
			leakedAt := time.Now().Add(-VMI_RECREATION_GRACE_PERIOD - time.Minute)
			a.leakedAt = &leakedAt

			Expect(c.vmiAllocationIsValid(a, false)).To(BeFalse())
		})
	})

	Context("when migration is completed", func() {
		It("should not consider succeeded migration as active", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := vmiName
			vmUID := "vm-uid"
			differentVmiUID := "different-uid"

			// Create VMI with different UID (simulating VMI recreation)
			vmi := createVMIWithOwner(vmiName, ns, differentVmiUID, vmName, vmUID)
			fakeVirtClient.AddVMI(vmi)

			a := createVMIAllocation(ns, vmiName, vmiUID)
			a.attrs[ipam.AttributeVM] = vmName
			a.attrs[ipam.AttributeVMUID] = vmUID

			vm := createVM(vmName, ns, vmUID)
			fakeVirtClient.AddVM(vm)

			// Set leakedAt to beyond grace period
			leakedAt := time.Now().Add(-VMI_RECREATION_GRACE_PERIOD - time.Minute)
			a.leakedAt = &leakedAt

			// Add a succeeded migration (should not count as active)
			migration := &kubevirtv1.VirtualMachineInstanceMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migration",
					Namespace: ns,
				},
				Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
					VMIName: vmiName,
				},
				Status: kubevirtv1.VirtualMachineInstanceMigrationStatus{
					Phase: kubevirtv1.MigrationSucceeded,
				},
			}
			fakeVirtClient.AddMigration(migration)

			// Should return false because migration has succeeded
			Expect(c.vmiAllocationIsValid(a, false)).To(BeFalse())
		})

		It("should not consider failed migration as active", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := vmiName
			vmUID := "vm-uid"
			differentVmiUID := "different-uid"

			// Create VMI with different UID (simulating VMI recreation)
			vmi := createVMIWithOwner(vmiName, ns, differentVmiUID, vmName, vmUID)
			fakeVirtClient.AddVMI(vmi)

			a := createVMIAllocation(ns, vmiName, vmiUID)
			a.attrs[ipam.AttributeVM] = vmName
			a.attrs[ipam.AttributeVMUID] = vmUID

			vm := createVM(vmName, ns, vmUID)
			fakeVirtClient.AddVM(vm)

			// Set leakedAt to beyond grace period
			leakedAt := time.Now().Add(-VMI_RECREATION_GRACE_PERIOD - time.Minute)
			a.leakedAt = &leakedAt

			// Add a failed migration (should not count as active)
			migration := &kubevirtv1.VirtualMachineInstanceMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migration",
					Namespace: ns,
				},
				Spec: kubevirtv1.VirtualMachineInstanceMigrationSpec{
					VMIName: vmiName,
				},
				Status: kubevirtv1.VirtualMachineInstanceMigrationStatus{
					Phase: kubevirtv1.MigrationFailed,
				},
			}
			fakeVirtClient.AddMigration(migration)

			// Should return false because migration has failed
			Expect(c.vmiAllocationIsValid(a, false)).To(BeFalse())
		})
	})

	Context("when resolving VM via stored attributes", func() {
		It("should resolve VM via stored attributes when VMI doesn't exist", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := "test-vm"
			vmUID := "vm-uid"

			// No VMI, but create VM with stored attributes
			a := createVMIAllocation(ns, vmiName, vmiUID)
			a.attrs[ipam.AttributeVM] = vmName
			a.attrs[ipam.AttributeVMUID] = vmUID

			vm := createVM(vmName, ns, vmUID)
			fakeVirtClient.AddVM(vm)

			// Create VMI with matching UID
			vmi := createVMI(vmiName, ns, vmiUID)
			fakeVirtClient.AddVMI(vmi)

			Expect(c.vmiAllocationIsValid(a, false)).To(BeTrue())
		})

		It("should return false when stored VM UID doesn't match", func() {
			ns := "test-ns"
			vmiName := "test-vmi"
			vmiUID := "test-uid"
			vmName := "test-vm"
			vmUID := "vm-uid"
			wrongVMUID := "wrong-vm-uid"

			// No VMI, but create VM with different UID than stored
			a := createVMIAllocation(ns, vmiName, vmiUID)
			a.attrs[ipam.AttributeVM] = vmName
			a.attrs[ipam.AttributeVMUID] = vmUID

			vm := createVM(vmName, ns, wrongVMUID)
			fakeVirtClient.AddVM(vm)

			// First check starts grace period
			result := c.vmiAllocationIsValid(a, false)
			// Should be true on first check (grace period starts)
			Expect(result).To(BeTrue())

			// Set leakedAt to beyond grace period
			leakedAt := time.Now().Add(-VMI_RECREATION_GRACE_PERIOD - time.Minute)
			a.leakedAt = &leakedAt

			// Now should be false
			Expect(c.vmiAllocationIsValid(a, false)).To(BeFalse())
		})
	})
})

// Helper function to create bool pointers
func boolPtr(b bool) *bool {
	return &b
}
