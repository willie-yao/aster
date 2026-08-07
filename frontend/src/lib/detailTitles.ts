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
  pattern: RegExp;
  allowed: (labels: string[]) => boolean;
}

const prefixRules: PrefixRule[] = [
  {
    pattern: /^Workload cluster creation\s+Creating\s+(?:a\s+)?/iu,
    allowed: () => true,
  },
  {
    pattern: /^Running the Cluster API E2E tests\s+/iu,
    allowed: () => true,
  },
  {
    pattern: /^Conformance Tests\s+/iu,
    allowed: () => true,
  },
  {
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
    if (!rule.allowed(labels)) continue;
    const match = displayName.match(rule.pattern);
    if (!match) continue;
    displayName = displayName.slice(match[0].length).trim();
    removedPrefixes.push(match[0].trim());
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
