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

export interface Manifest {
  id: string;
  name: string;
  short_name?: string;
  source: Source;
  testgrid: TestGrid;
  storage: Storage;
  branding: Branding;
  categories?: CategoryRule[];
  category_display_order?: string[];
  ai?: AIManifest;
  // Display-only hint derived at fetch time: the longest periodic-<x>- prefix
  // shared by a majority of discovered periodic jobs. Used by shortJobName to
  // strip boilerplate from job names in the UI.
  short_name_prefix?: string;
}
