package pool

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
)

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

func TestIsRunning(t *testing.T) {
	succeeded := "Succeeded"
	failed := "Failed"

	cases := []struct {
		name string
		vm   *armcompute.VirtualMachineScaleSetVM
		want bool
	}{
		{
			name: "nil properties",
			vm:   &armcompute.VirtualMachineScaleSetVM{},
			want: false,
		},
		{
			name: "not provisioned successfully",
			vm: &armcompute.VirtualMachineScaleSetVM{
				Properties: &armcompute.VirtualMachineScaleSetVMProperties{
					ProvisioningState: &failed,
				},
			},
			want: false,
		},
		{
			name: "provisioned but no instance view",
			vm: &armcompute.VirtualMachineScaleSetVM{
				Properties: &armcompute.VirtualMachineScaleSetVMProperties{
					ProvisioningState: &succeeded,
				},
			},
			want: false,
		},
		{
			name: "provisioned and running",
			vm: &armcompute.VirtualMachineScaleSetVM{
				Properties: &armcompute.VirtualMachineScaleSetVMProperties{
					ProvisioningState: &succeeded,
					InstanceView: &armcompute.VirtualMachineScaleSetVMInstanceView{
						Statuses: []*armcompute.InstanceViewStatus{
							{Code: strPtr("ProvisioningState/succeeded")},
							{Code: strPtr("PowerState/running")},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "provisioned but stopped",
			vm: &armcompute.VirtualMachineScaleSetVM{
				Properties: &armcompute.VirtualMachineScaleSetVMProperties{
					ProvisioningState: &succeeded,
					InstanceView: &armcompute.VirtualMachineScaleSetVMInstanceView{
						Statuses: []*armcompute.InstanceViewStatus{
							{Code: strPtr("PowerState/stopped")},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "power state code casing is handled case-insensitively",
			vm: &armcompute.VirtualMachineScaleSetVM{
				Properties: &armcompute.VirtualMachineScaleSetVMProperties{
					ProvisioningState: &succeeded,
					InstanceView: &armcompute.VirtualMachineScaleSetVMInstanceView{
						Statuses: []*armcompute.InstanceViewStatus{
							{Code: strPtr("powerstate/RUNNING")},
						},
					},
				},
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRunning(tc.vm); got != tc.want {
				t.Errorf("isRunning() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInstanceIDFromVMResourceID(t *testing.T) {
	cases := []struct {
		name string
		vm   *armnetwork.SubResource
		want string
	}{
		{name: "nil SubResource", vm: nil, want: ""},
		{name: "nil ID", vm: &armnetwork.SubResource{}, want: ""},
		{
			name: "well-formed VMSS VM resource ID",
			vm: &armnetwork.SubResource{
				ID: strPtr("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss-web/virtualMachines/3"),
			},
			want: "3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := instanceIDFromVMResourceID(tc.vm); got != tc.want {
				t.Errorf("instanceIDFromVMResourceID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrimaryPrivateIP(t *testing.T) {
	cases := []struct {
		name    string
		configs []*armnetwork.InterfaceIPConfiguration
		want    string
	}{
		{name: "no configs", configs: nil, want: ""},
		{
			name: "single config with no Primary flag falls back",
			configs: []*armnetwork.InterfaceIPConfiguration{
				{Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					PrivateIPAddress: strPtr("10.0.0.4"),
				}},
			},
			want: "10.0.0.4",
		},
		{
			name: "prefers the config explicitly marked primary",
			configs: []*armnetwork.InterfaceIPConfiguration{
				{Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					PrivateIPAddress: strPtr("10.0.0.5"),
					Primary:          boolPtr(false),
				}},
				{Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					PrivateIPAddress: strPtr("10.0.0.6"),
					Primary:          boolPtr(true),
				}},
			},
			want: "10.0.0.6",
		},
		{
			name: "skips configs with no private IP",
			configs: []*armnetwork.InterfaceIPConfiguration{
				{Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{}},
				{Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					PrivateIPAddress: strPtr("10.0.0.7"),
				}},
			},
			want: "10.0.0.7",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := primaryPrivateIP(tc.configs); got != tc.want {
				t.Errorf("primaryPrivateIP() = %q, want %q", got, tc.want)
			}
		})
	}
}
