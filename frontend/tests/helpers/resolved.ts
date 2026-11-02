import type { Plugin } from "vite";
import type { ResolvedReadState, ResolutionAvailability } from "../../src/lib/resolvedState.js";

export interface ResolvedFixture {
  state: ResolvedReadState & { availability: ResolutionAvailability };
}

export const resolvedFixturePlugin: Plugin = {
  name: "resolved-test-data",
  load(id) {
    if (!id.endsWith("/src/hooks/useData.ts")) return;
    return `
      export const resolvedFixture = {
        state: { data: { resolved: {}, causes: {} }, loading: false, error: null, availability: "available" }
      };
      export const useResolved = () => ({ ...resolvedFixture.state, refetch: () => {} });
    `;
  },
};
