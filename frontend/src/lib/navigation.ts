// Primary navigation destinations, split into the public signal set and the
// operator set. Operator destinations are gated on server capabilities so a
// signed-out visitor is never offered a page that only renders a sign-in wall.
export type NavScope = "signal" | "operator";

export interface NavDestination {
  id: string;
  to: string;
  /** Short label shown under the rail icon. */
  label: string;
  /** Full destination name for assistive technology. */
  title: string;
  scope: NavScope;
  active: boolean;
}

export interface NavDestinationInput {
  pathname: string;
  pullRequestsEnabled: boolean;
  analysisHealthEnabled: boolean;
  aiUsageEnabled: boolean;
  /**
   * Whether the viewer is signed in as an operator. Operator pages render only
   * a sign-in wall without it, so the deployment flag alone is not enough to
   * offer the destination.
   */
  operatorAccess: boolean;
}

export function navDestinations({
  pathname,
  pullRequestsEnabled,
  analysisHealthEnabled,
  aiUsageEnabled,
  operatorAccess,
}: NavDestinationInput): NavDestination[] {
  const flakyActive = pathname === "/flaky" || pathname.startsWith("/flaky/");
  const pullRequestsActive =
    pathname === "/pull-requests" || pathname.startsWith("/pull-requests/");
  const healthActive = pathname === "/analysis-health";
  const usageActive = pathname === "/ai-usage";

  const destinations: NavDestination[] = [
    {
      id: "overview",
      to: "/",
      label: "Overview",
      title: "Overview",
      scope: "signal",
      active: !flakyActive && !pullRequestsActive && !healthActive && !usageActive,
    },
    {
      id: "flaky",
      to: "/flaky",
      label: "Trends",
      title: "Failure Trends",
      scope: "signal",
      active: flakyActive,
    },
  ];

  if (pullRequestsEnabled) {
    destinations.push({
      id: "pull-requests",
      to: "/pull-requests",
      label: "Pulls",
      title: "Pull Requests",
      scope: "signal",
      active: pullRequestsActive,
    });
  }
  if (analysisHealthEnabled && operatorAccess) {
    destinations.push({
      id: "analysis-health",
      to: "/analysis-health",
      label: "Health",
      title: "Analysis Health",
      scope: "operator",
      active: healthActive,
    });
  }
  if (aiUsageEnabled && operatorAccess) {
    destinations.push({
      id: "ai-usage",
      to: "/ai-usage",
      label: "Usage",
      title: "AI Usage",
      scope: "operator",
      active: usageActive,
    });
  }
  return destinations;
}
