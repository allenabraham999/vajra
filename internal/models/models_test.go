package models

import (
	"reflect"
	"testing"
)

func TestNodeState_Valid(t *testing.T) {
	good := []NodeState{
		NodeStateRegistering, NodeStateActive, NodeStateDraining,
		NodeStateQuarantined, NodeStateOffline, NodeStateDecommissioned,
	}
	for _, s := range good {
		if !s.Valid() {
			t.Errorf("expected %q to be Valid", s)
		}
	}
	for _, bad := range []NodeState{"", "active", "ONLINE"} {
		if bad.Valid() {
			t.Errorf("expected %q to be invalid", bad)
		}
	}
}

func TestClusterState_Valid(t *testing.T) {
	good := []ClusterState{ClusterStateActive, ClusterStateDraining, ClusterStateOffline}
	for _, s := range good {
		if !s.Valid() {
			t.Errorf("expected %q to be Valid", s)
		}
	}
	for _, bad := range []ClusterState{"", "active", "PROVISIONING"} {
		if bad.Valid() {
			t.Errorf("expected %q to be invalid", bad)
		}
	}
}

func TestOperationType_Valid(t *testing.T) {
	good := []OperationType{
		OperationTypeCreate, OperationTypeStop, OperationTypeStart,
		OperationTypeDestroy, OperationTypeSnapshot, OperationTypeRestore,
		OperationTypeClone, OperationTypeMigrate, OperationTypeArchive,
	}
	for _, s := range good {
		if !s.Valid() {
			t.Errorf("expected %q to be Valid", s)
		}
	}
	for _, bad := range []OperationType{"", "create", "FOO"} {
		if bad.Valid() {
			t.Errorf("expected %q to be invalid", bad)
		}
	}
}

func TestOperationStatus_Valid(t *testing.T) {
	good := []OperationStatus{
		OperationStatusPending, OperationStatusInProgress,
		OperationStatusCompleted, OperationStatusFailed,
	}
	for _, s := range good {
		if !s.Valid() {
			t.Errorf("expected %q to be Valid", s)
		}
	}
	for _, bad := range []OperationStatus{"", "pending", "DONE"} {
		if bad.Valid() {
			t.Errorf("expected %q to be invalid", bad)
		}
	}
}

func TestSandboxConfig_RoundTrip(t *testing.T) {
	want := SandboxConfig{VCPUs: 4, MemoryMB: 2048, DiskGB: 20}
	v, err := want.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	var got SandboxConfig
	if err := got.Scan(v); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got != want {
		t.Fatalf("roundtrip mismatch: got %+v, want %+v", got, want)
	}
}

func TestNodeCapacity_RoundTrip(t *testing.T) {
	want := NodeCapacity{TotalCPU: 32, TotalMemoryMB: 65536, TotalDiskGB: 1000}
	v, err := want.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	var got NodeCapacity
	if err := got.Scan(v); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got != want {
		t.Fatalf("roundtrip mismatch: got %+v, want %+v", got, want)
	}
}

func TestNodeUsage_RoundTrip(t *testing.T) {
	want := NodeUsage{UsedCPU: 8, UsedMemoryMB: 4096, UsedDiskGB: 100}
	v, err := want.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	var got NodeUsage
	if err := got.Scan(v); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got != want {
		t.Fatalf("roundtrip mismatch: got %+v, want %+v", got, want)
	}
}

func TestPermissions_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   Permissions
		want Permissions
	}{
		{"non-empty", Permissions{"sandbox:read", "sandbox:write"}, Permissions{"sandbox:read", "sandbox:write"}},
		{"empty", Permissions{}, Permissions{}},
		{"nil normalizes to empty", nil, Permissions{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := c.in.Value()
			if err != nil {
				t.Fatalf("Value: %v", err)
			}
			var got Permissions
			if err := got.Scan(v); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestJSONScannerHandlesNullAndStringSource(t *testing.T) {
	var c SandboxConfig
	if err := c.Scan(nil); err != nil {
		t.Fatalf("nil source should be no-op: %v", err)
	}
	if (c != SandboxConfig{}) {
		t.Fatalf("nil scan mutated the value: %+v", c)
	}
	// some drivers return string, not []byte
	if err := c.Scan(`{"vcpus":2,"memory_mb":512,"disk_gb":10}`); err != nil {
		t.Fatalf("string source: %v", err)
	}
	if want := (SandboxConfig{VCPUs: 2, MemoryMB: 512, DiskGB: 10}); c != want {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestJSONScannerRejectsUnsupportedSource(t *testing.T) {
	var c SandboxConfig
	if err := c.Scan(42); err == nil {
		t.Fatal("expected error scanning int into JSONB column")
	}
}

func TestValidateNodeIP(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		wantErr bool
	}{
		{"ipv4", "10.0.0.1", false},
		{"ipv6", "fe80::1", false},
		{"localhost ipv6", "::1", false},
		{"empty", "", true},
		{"hostname", "node-01.local", true},
		{"junk", "not-an-ip", true},
		{"trailing dot", "10.0.0.1.", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateNodeIP(c.ip)
			if (err != nil) != c.wantErr {
				t.Fatalf("ValidateNodeIP(%q) err=%v, wantErr=%v", c.ip, err, c.wantErr)
			}
		})
	}
}
