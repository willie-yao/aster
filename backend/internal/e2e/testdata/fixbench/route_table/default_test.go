package routetable

import "testing"

func TestDefaultControlPlaneRouteTablePreservesExplicitValue(t *testing.T) {
	spec := &NetworkSpec{ControlPlaneRouteTable: "control-plane", NodeRouteTable: "node"}
	DefaultControlPlaneRouteTable(spec)
	if spec.ControlPlaneRouteTable != "control-plane" {
		t.Fatalf("control-plane route table = %q", spec.ControlPlaneRouteTable)
	}
}
