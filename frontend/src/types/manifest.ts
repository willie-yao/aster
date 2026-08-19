// Manifest mirrors the backend project config JSON. The fetcher writes it to
// data/manifest.json on every run, and ManifestProvider loads it once at app
// boot.

export interface SourceRepo {
  owner: string;
  name: string;
}

export interface Branding {
  title: string;
  base_path: string;
  site_url: string;
  source_repo: SourceRepo;
}

export interface Source {
  include_presubmits?: boolean;
}

export interface TestGrid {
  dashboard: string;
}

// PullRequestsManifest mirrors the optional pull_requests config block. The
// nav tab and pull request routes are shown only when it is enabled.
export interface PullRequestsManifest {
  enabled: boolean;
  max?: number;
  builds_per_job?: number;
}

export interface Storage {
  provider: string;
  bucket: string;
  base?: string;
  web_base?: string;
  prow_base?: string;
}

export interface CategoryRule {
  match: string;
  id: string;
  label: string;
}

export interface SkillBundleManifest {
  profiles: string[];
  engine_count: number;
  consumer_count: number;
  consumer_bundle_present: boolean;
  hash?: string;
}

export interface AIManifest {
  source_repo?: SourceRepo;
  skill_bundle?: SkillBundleManifest;
}

export interface LowPassRateManifest {
  // Exclusive pass-rate cutoff in [0, 1]. 1 selects every test that failed at
  // least once; 0 selects none.
  threshold: number;
  min_runs?: number;
  recent_runs?: number;
  max_items?: number;
}

export interface AttentionManifest {
  persistent_after?: number;
  low_pass_rate?: LowPassRateManifest;
}

export interface Manifest {
  id: string;
  name: string;
  short_name?: string;
  source: Source;
  testgrid: TestGrid;
  storage: Storage;
  branding: Branding;
  pull_requests?: PullRequestsManifest;
  categories?: CategoryRule[];
  category_display_order?: string[];
  ai?: AIManifest;
  attention?: AttentionManifest;
  // Display-only hint derived at fetch time: the longest periodic-<x>- prefix
  // shared by a majority of discovered periodic jobs. Used by shortJobName to
  // strip boilerplate from job names in the UI.
  short_name_prefix?: string;
}
