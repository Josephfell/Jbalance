package pool

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
)

// AzureVMSSGroup maps a control-plane group name to the Azure VM Scale Set
// that backs it, along with the port each instance in that scale set
// serves traffic on.
type AzureVMSSGroup struct {
	// Group is the control-plane group name, e.g. "web-tier". Must match
	// what data plane instances subscribe to.
	Group string
	// ScaleSetName is the name of the Azure VM Scale Set.
	ScaleSetName string
	// Port is the port number the application on each instance listens
	// on. The provider reports "<private-ip>:<port>" as each backend's
	// address.
	Port int
	// Weight is applied to every backend reported for this group. Leave
	// zero to default to 1 (equal weighting).
	Weight int32
}

// AzureVMSSProvider implements Provider by querying Azure VM Scale Set
// instances and their network interfaces directly via the Azure SDK for
// Go. Only instances that are both successfully provisioned and reporting
// PowerState/running are reported as backends — anything still starting,
// stopping, or deallocated is excluded.
//
// Authentication uses azidentity.DefaultAzureCredential, which supports
// (in order of preference) environment variables, workload identity,
// managed identity, and Azure CLI credentials — see the README for how to
// configure credentials for local testing vs. running as an Azure
// resource.
type AzureVMSSProvider struct {
	subscriptionID    string
	resourceGroupName string
	groups            map[string]AzureVMSSGroup

	vmClient  *armcompute.VirtualMachineScaleSetVMsClient
	nicClient *armnetwork.InterfacesClient
}

// NewAzureVMSSProvider creates a provider backed by one or more VM Scale
// Sets in a single resource group. cred is typically the result of
// azidentity.NewDefaultAzureCredential(nil).
func NewAzureVMSSProvider(subscriptionID, resourceGroupName string, groups []AzureVMSSGroup, cred azcore.TokenCredential) (*AzureVMSSProvider, error) {
	vmClient, err := armcompute.NewVirtualMachineScaleSetVMsClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("pool: failed to create VMSS VMs client: %w", err)
	}

	nicClient, err := armnetwork.NewInterfacesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("pool: failed to create network interfaces client: %w", err)
	}

	groupMap := make(map[string]AzureVMSSGroup, len(groups))
	for _, g := range groups {
		if g.Weight <= 0 {
			g.Weight = 1
		}
		groupMap[g.Group] = g
	}

	return &AzureVMSSProvider{
		subscriptionID:    subscriptionID,
		resourceGroupName: resourceGroupName,
		groups:            groupMap,
		vmClient:          vmClient,
		nicClient:         nicClient,
	}, nil
}

// Groups implements Provider.
func (p *AzureVMSSProvider) Groups(_ context.Context) ([]string, error) {
	names := make([]string, 0, len(p.groups))
	for name := range p.groups {
		names = append(names, name)
	}
	return names, nil
}

// Snapshot implements Provider. It lists all VM instances in the group's
// scale set with their instance view expanded (to get power state) in a
// single paged call, filters to running+provisioned instances, then lists
// the scale set's network interfaces in a second paged call and matches
// each running instance to its private IP by NIC resource ID.
//
// Two Azure API calls per group per reconcile tick, regardless of instance
// count — this scales to large scale sets without an N+1 call pattern.
func (p *AzureVMSSProvider) Snapshot(ctx context.Context, group string) (Snapshot, error) {
	cfg, ok := p.groups[group]
	if !ok {
		return Snapshot{}, fmt.Errorf("pool: unknown group %q", group)
	}

	runningInstanceIDs, err := p.listRunningInstanceIDs(ctx, cfg.ScaleSetName)
	if err != nil {
		return Snapshot{}, fmt.Errorf("pool: failed to list VMSS instances for %q: %w", cfg.ScaleSetName, err)
	}

	privateIPsByInstanceID, err := p.listPrivateIPs(ctx, cfg.ScaleSetName)
	if err != nil {
		return Snapshot{}, fmt.Errorf("pool: failed to list network interfaces for %q: %w", cfg.ScaleSetName, err)
	}

	backends := make([]Backend, 0, len(runningInstanceIDs))
	for _, instanceID := range runningInstanceIDs {
		ip, ok := privateIPsByInstanceID[instanceID]
		if !ok || ip == "" {
			log.Printf("pool: azurevmss: instance %s in scale set %s has no private IP yet, skipping", instanceID, cfg.ScaleSetName)
			continue
		}
		backends = append(backends, Backend{
			Address: fmt.Sprintf("%s:%d", ip, cfg.Port),
			Weight:  cfg.Weight,
		})
	}

	return Snapshot{Group: group, Backends: backends}, nil
}

// listRunningInstanceIDs returns the instance IDs of every VM in the scale
// set that is both provisioned successfully and currently powered on.
func (p *AzureVMSSProvider) listRunningInstanceIDs(ctx context.Context, scaleSetName string) ([]string, error) {
	var ids []string

	expand := "instanceView"
	pager := p.vmClient.NewListPager(p.resourceGroupName, scaleSetName, &armcompute.VirtualMachineScaleSetVMsClientListOptions{
		Expand: &expand,
	})

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, vm := range page.Value {
			if vm == nil || vm.InstanceID == nil {
				continue
			}
			if isRunning(vm) {
				ids = append(ids, *vm.InstanceID)
			}
		}
	}

	return ids, nil
}

// isRunning reports whether a VMSS VM is successfully provisioned and its
// instance view reports PowerState/running.
func isRunning(vm *armcompute.VirtualMachineScaleSetVM) bool {
	if vm.Properties == nil {
		return false
	}
	if vm.Properties.ProvisioningState == nil || *vm.Properties.ProvisioningState != "Succeeded" {
		return false
	}
	if vm.Properties.InstanceView == nil {
		return false
	}
	for _, status := range vm.Properties.InstanceView.Statuses {
		if status == nil || status.Code == nil {
			continue
		}
		if strings.EqualFold(*status.Code, "PowerState/running") {
			return true
		}
	}
	return false
}

// listPrivateIPs returns a map of VMSS instance ID -> private IP address,
// derived from the scale set's network interfaces. An instance ID is
// extracted from each NIC's VirtualMachine SubResource ID, which has the
// form ".../virtualMachineScaleSets/<name>/virtualMachines/<instanceId>/...".
func (p *AzureVMSSProvider) listPrivateIPs(ctx context.Context, scaleSetName string) (map[string]string, error) {
	result := make(map[string]string)

	pager := p.nicClient.NewListVirtualMachineScaleSetNetworkInterfacesPager(p.resourceGroupName, scaleSetName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, nic := range page.Value {
			if nic == nil || nic.Properties == nil {
				continue
			}
			instanceID := instanceIDFromVMResourceID(nic.Properties.VirtualMachine)
			if instanceID == "" {
				continue
			}
			ip := primaryPrivateIP(nic.Properties.IPConfigurations)
			if ip != "" {
				result[instanceID] = ip
			}
		}
	}

	return result, nil
}

// instanceIDFromVMResourceID extracts the trailing instance ID segment
// from a NIC's associated VirtualMachine resource ID.
func instanceIDFromVMResourceID(vm *armnetwork.SubResource) string {
	if vm == nil || vm.ID == nil {
		return ""
	}
	parts := strings.Split(*vm.ID, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// primaryPrivateIP returns the private IP of the primary IP configuration,
// or the first one found if none is explicitly marked primary.
func primaryPrivateIP(configs []*armnetwork.InterfaceIPConfiguration) string {
	var fallback string
	for _, cfg := range configs {
		if cfg == nil || cfg.Properties == nil || cfg.Properties.PrivateIPAddress == nil {
			continue
		}
		ip := *cfg.Properties.PrivateIPAddress
		if cfg.Properties.Primary != nil && *cfg.Properties.Primary {
			return ip
		}
		if fallback == "" {
			fallback = ip
		}
	}
	return fallback
}
