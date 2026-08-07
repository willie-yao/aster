export interface TestDisplayName {
  displayName: string;
  labels: string[];
  removedPrefixes: string[];
  usedFallback: boolean;
}

const exactLabels = new Set([
  "It",
  "DRA",
  "Alpha",
  "Beta",
  "Serial",
  "Slow",
  "Conformance",
  "NodeConformance",
]);

function isStructuredLabel(value: string): boolean {
  return exactLabels.has(value) ||
    value.startsWith("sig-") ||
    value.startsWith("Feature:") ||
    value.startsWith("FeatureGate:") ||
    value.startsWith("KubeletMinVersion:");
}

interface PrefixRule {
  name: string;
  pattern: RegExp;
  allowed: (labels: string[]) => boolean;
}

const prefixRules: PrefixRule[] = [
  {
    name: "Workload cluster creation Creating",
    pattern: /^Workload cluster creation\s+Creating\s+(?:a\s+)?/iu,
    allowed: () => true,
  },
  {
    name: "Running the Cluster API E2E tests",
    pattern: /^Running the Cluster API E2E tests\s+/iu,
    allowed: () => true,
  },
  {
    name: "Conformance Tests",
    pattern: /^Conformance Tests\s+/iu,
    allowed: () => true,
  },
  {
    name: "Running",
    pattern: /^Running\s+/iu,
    allowed: () => true,
  },
  {
    name: "kubelet",
    pattern: /^kubelet\s+/iu,
    allowed: (labels) =>
      labels.includes("[DRA]") && labels.some((label) => label.startsWith("[FeatureGate:")),
  },
];

function capitalizeFirst(value: string): string {
  const characters = Array.from(value);
  if (characters.length === 0) return value;
  return characters[0].toLocaleUpperCase() + characters.slice(1).join("");
}

export function parseTestDisplayName(canonicalName: string): TestDisplayName {
  const labels: string[] = [];
  const withoutLabels = canonicalName.replace(/\[([^\]]+)\]/gu, (match, value: string) => {
    if (!isStructuredLabel(value)) return match;
    labels.push(match);
    return " ";
  });
  let displayName = withoutLabels.replace(/\s+/gu, " ").trim();
  const removedPrefixes: string[] = [];

  for (const rule of prefixRules) {
    if (!rule.allowed(labels) || !rule.pattern.test(displayName)) continue;
    displayName = displayName.replace(rule.pattern, "").trim();
    removedPrefixes.push(rule.name);
    break;
  }

  if (!displayName) {
    return {
      displayName: canonicalName,
      labels,
      removedPrefixes,
      usedFallback: true,
    };
  }

  return {
    displayName: capitalizeFirst(displayName),
    labels,
    removedPrefixes,
    usedFallback: false,
  };
}
