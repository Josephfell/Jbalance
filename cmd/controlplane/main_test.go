package main

import (
	"testing"

	"github.com/josephfell/go-loadbalancer/internal/pool"
)

func TestParseAzureVMSSGroups_Empty(t *testing.T) {
	groups, err := parseAzureVMSSGroups("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("expected no groups for empty spec, got %d", len(groups))
	}
}

func TestParseAzureVMSSGroups_SingleGroupDefaultWeight(t *testing.T) {
	groups, err := parseAzureVMSSGroups("web-tier:vmss-web:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	want := pool.AzureVMSSGroup{Group: "web-tier", ScaleSetName: "vmss-web", Port: 8080, Weight: 1}
	if groups[0] != want {
		t.Errorf("got %+v, want %+v", groups[0], want)
	}
}

func TestParseAzureVMSSGroups_MultipleGroupsWithWeight(t *testing.T) {
	groups, err := parseAzureVMSSGroups("web-tier:vmss-web:8080:2,api-tier:vmss-api:8081")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0].Weight != 2 {
		t.Errorf("expected first group weight 2, got %d", groups[0].Weight)
	}
	if groups[1].Weight != 1 {
		t.Errorf("expected second group weight to default to 1, got %d", groups[1].Weight)
	}
}

func TestParseAzureVMSSGroups_IgnoresBlankEntries(t *testing.T) {
	groups, err := parseAzureVMSSGroups("web-tier:vmss-web:8080,,  ,api-tier:vmss-api:8081")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 2 {
		t.Errorf("expected blank entries to be skipped, got %d groups", len(groups))
	}
}

func TestParseAzureVMSSGroups_InvalidFormat(t *testing.T) {
	cases := []string{
		"missing-port-and-scaleset",
		"group:scaleset",                 // too few parts
		"group:scaleset:port:weight:huh", // too many parts
	}
	for _, c := range cases {
		if _, err := parseAzureVMSSGroups(c); err == nil {
			t.Errorf("expected an error for invalid spec %q, got nil", c)
		}
	}
}

func TestParseAzureVMSSGroups_InvalidPort(t *testing.T) {
	if _, err := parseAzureVMSSGroups("web-tier:vmss-web:not-a-port"); err == nil {
		t.Error("expected an error for a non-numeric port, got nil")
	}
}

func TestParseAzureVMSSGroups_InvalidWeight(t *testing.T) {
	if _, err := parseAzureVMSSGroups("web-tier:vmss-web:8080:not-a-weight"); err == nil {
		t.Error("expected an error for a non-numeric weight, got nil")
	}
}

func TestBuildProvider_UnknownKind(t *testing.T) {
	_, _, err := buildProvider(t.Context(), "does-not-exist", providerConfig{})
	if err == nil {
		t.Error("expected an error for an unknown provider kind, got nil")
	}
}

func TestBuildProvider_AzureVMSSRequiresSubscriptionID(t *testing.T) {
	_, _, err := buildProvider(t.Context(), "azure-vmss", providerConfig{
		azureResourceGroup: "rg",
		azureVMSSGroups:    "web-tier:vmss-web:8080",
	})
	if err == nil {
		t.Error("expected an error when azureSubscriptionID is missing, got nil")
	}
}

func TestBuildProvider_AzureVMSSRequiresResourceGroup(t *testing.T) {
	_, _, err := buildProvider(t.Context(), "azure-vmss", providerConfig{
		azureSubscriptionID: "sub",
		azureVMSSGroups:     "web-tier:vmss-web:8080",
	})
	if err == nil {
		t.Error("expected an error when azureResourceGroup is missing, got nil")
	}
}

func TestBuildProvider_AzureVMSSRequiresAtLeastOneGroup(t *testing.T) {
	_, _, err := buildProvider(t.Context(), "azure-vmss", providerConfig{
		azureSubscriptionID: "sub",
		azureResourceGroup:  "rg",
	})
	if err == nil {
		t.Error("expected an error when no groups are specified, got nil")
	}
}

func TestBuildProvider_Fake(t *testing.T) {
	provider, cleanup, err := buildProvider(t.Context(), "fake", providerConfig{
		fakeBasePort:    9000,
		simulateScaling: false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	groups, err := provider.Groups(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) == 0 {
		t.Error("expected the fake provider to report at least one group")
	}
}
