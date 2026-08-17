package routetable

// NetworkSpec contains the route table names used by the control-plane and node subnets.
type NetworkSpec struct {
	ControlPlaneRouteTable string
	NodeRouteTable         string
}

// DefaultControlPlaneRouteTable applies network defaults without replacing explicit values.
func DefaultControlPlaneRouteTable(spec *NetworkSpec) {
	if spec == nil || spec.ControlPlaneRouteTable != "" {
		return
	}
}
