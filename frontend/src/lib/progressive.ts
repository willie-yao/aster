export function initialProgressiveCount(total: number, batchSize: number): number {
  return Math.min(total, batchSize);
}

export function nextProgressiveCount(
  current: number,
  total: number,
  batchSize: number,
): number {
  return Math.min(total, current + batchSize);
}

export function trailingWindowStart(total: number, visible: number): number {
  return Math.max(0, total - visible);
}
