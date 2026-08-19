import { useEffect, useState, type ReactNode } from "react";
import Box from "@mui/material/Box";
import Typography from "@mui/material/Typography";
import { ManifestContext } from "../hooks/useManifest";
import type { Manifest } from "../types/manifest";

export function ManifestProvider({ children }: { children: ReactNode }) {
  const [manifest, setManifest] = useState<Manifest | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const url = `${import.meta.env.BASE_URL}data/manifest.json`;
    fetch(url)
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json() as Promise<Manifest>;
      })
      .then((m) => {
        setManifest(m);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  if (error) {
    return (
      <Box
        sx={{
          minHeight: "100vh",
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          p: 3,
        }}
      >
        <Box sx={{ maxWidth: 420, textAlign: "center" }}>
          <Typography variant="h6" component="h1" gutterBottom>
            Failed to load dashboard config
          </Typography>
          <Typography variant="body2" color="textSecondary">
            {error}
          </Typography>
          <Typography variant="body2" color="textSecondary" sx={{ mt: 1 }}>
            Expected file:{" "}
            <Box component="code" sx={{ fontFamily: "monospace" }}>
              data/manifest.json
            </Box>
          </Typography>
        </Box>
      </Box>
    );
  }

  if (!manifest) {
    return null;
  }

  return <ManifestContext.Provider value={manifest}>{children}</ManifestContext.Provider>;
}
