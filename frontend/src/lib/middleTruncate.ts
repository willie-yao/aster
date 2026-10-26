// splitForMiddleTruncation separates a long identifier into a head that may be
// ellipsised and a tail that always stays visible, so names that differ only in
// their suffix, such as release-1-26 and release-1-27, remain distinguishable.
export function splitForMiddleTruncation(name: string, tailLength = 16): { head: string; tail: string } {
  if (name.length <= tailLength + 4) return { head: name, tail: "" };
  return { head: name.slice(0, -tailLength), tail: name.slice(-tailLength) };
}
