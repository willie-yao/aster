package onboard

import (
	"fmt"
	"net/netip"
	"strings"
)

// KubernetesOriginPolicy contains the static Service exposure contract.
type KubernetesOriginPolicy struct {
	ServiceType              string
	SourceRanges             []string
	InternalEnabled          bool
	HasInternalAnnotations   bool
	PublicOriginAcknowledged bool
	Interactive              bool
}

// KubernetesOriginRestriction classifies configured LoadBalancer restrictions.
type KubernetesOriginRestriction string

const (
	KubernetesOriginNotLoadBalancer KubernetesOriginRestriction = "not-load-balancer"
	KubernetesOriginRestricted      KubernetesOriginRestriction = "restricted"
	KubernetesOriginUnverified      KubernetesOriginRestriction = "unverified"
)

// ClassifyKubernetesOrigin validates static exposure without proving live enforcement.
func ClassifyKubernetesOrigin(policy KubernetesOriginPolicy) (KubernetesOriginRestriction, error) {
	serviceType := policy.ServiceType
	if serviceType == "" {
		serviceType = "ClusterIP"
	}
	switch serviceType {
	case "ClusterIP", "LoadBalancer", "NodePort":
	default:
		return "", fmt.Errorf("server.service.type must be ClusterIP, LoadBalancer, or NodePort")
	}
	for _, cidr := range policy.SourceRanges {
		if strings.TrimSpace(cidr) == "" {
			return "", fmt.Errorf("server.service.loadBalancerSourceRanges must not contain empty entries")
		}
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return "", fmt.Errorf("server.service.loadBalancerSourceRanges contains invalid CIDR %q: %w", cidr, err)
		}
		if prefix.Bits() == 0 {
			return "", fmt.Errorf("server.service.loadBalancerSourceRanges must not contain universal CIDRs; remove them and set publicOriginAcknowledged=true for an intentional public origin")
		}
	}
	if len(policy.SourceRanges) > 0 && serviceType != "LoadBalancer" {
		return "", fmt.Errorf("server.service.loadBalancerSourceRanges requires server.service.type=LoadBalancer")
	}
	if policy.InternalEnabled && serviceType != "LoadBalancer" {
		return "", fmt.Errorf("server.service.internal.enabled requires server.service.type=LoadBalancer")
	}
	if policy.InternalEnabled && !policy.HasInternalAnnotations {
		return "", fmt.Errorf("server.service.internal.annotations is required when internal.enabled=true")
	}
	if policy.PublicOriginAcknowledged && serviceType != "LoadBalancer" {
		return "", fmt.Errorf("server.service.publicOriginAcknowledged applies only to LoadBalancer Services")
	}
	if serviceType != "LoadBalancer" {
		return KubernetesOriginNotLoadBalancer, nil
	}
	if policy.InternalEnabled || len(policy.SourceRanges) == 1 {
		return KubernetesOriginRestricted, nil
	}
	if policy.Interactive && !policy.PublicOriginAcknowledged {
		if len(policy.SourceRanges) > 1 {
			return "", fmt.Errorf("authenticated server features with multiple loadBalancerSourceRanges require publicOriginAcknowledged=true because their union is not proven restricted")
		}
		return "", fmt.Errorf("authenticated server features with a LoadBalancer require loadBalancerSourceRanges, internal.enabled, or publicOriginAcknowledged=true")
	}
	return KubernetesOriginUnverified, nil
}
