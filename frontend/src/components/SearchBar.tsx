import Search from "@mui/icons-material/Search";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import IconButton from "@mui/material/IconButton";
import InputAdornment from "@mui/material/InputAdornment";
import List from "@mui/material/List";
import ListItemButton from "@mui/material/ListItemButton";
import ListSubheader from "@mui/material/ListSubheader";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";
import { useState, useEffect, useRef, useMemo } from "react";
import { useNavigate } from "react-router-dom";
import Fuse from "fuse.js";
import { useSearchIndex } from "../hooks/useData";
import { useManifest } from "../hooks/useManifest";
import { shortJobName, shortTestName } from "../lib/utils";
import { soft } from "../theme";
import { Panel } from "./Panel";
import type { SearchEntry } from "../types/dashboard";

export function SearchBar() {
  const manifest = useManifest();
  const filePrefix = manifest.short_name_prefix ?? "";
  const [activated, setActivated] = useState(false);
  const { data, loading, error } = useSearchIndex(activated);
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState(false);
  const [mobileExpanded, setMobileExpanded] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);

  const fuse = useMemo(() => {
    if (!data?.entries) return null;
    return new Fuse(data.entries, {
      keys: ["test_name", "job_name", "tab_name"],
      threshold: 0.4,
      includeScore: true,
    });
  }, [data]);

  const results = useMemo(() => {
    if (!fuse || !query.trim()) return [];
    return fuse.search(query, { limit: 20 });
  }, [fuse, query]);

  // Group results by JobID so same-named presubmit and periodic jobs do not collide.
  const grouped = useMemo(() => {
    const groups = new Map<string, { jobName: string; items: { item: SearchEntry; score?: number }[] }>();
    for (const r of results) {
      const key = r.item.job_id;
      const existing = groups.get(key);
      if (existing) existing.items.push(r);
      else groups.set(key, { jobName: r.item.job_name, items: [r] });
    }
    return groups;
  }, [results]);

  // Global Cmd+K and Ctrl+K shortcut.
  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key === "k") {
        e.preventDefault();
        setActivated(true);
        setMobileExpanded(true);
        globalThis.setTimeout(() => inputRef.current?.focus(), 0);
      }
    }
    document.addEventListener("keydown", handleKeyDown);
    return () => document.removeEventListener("keydown", handleKeyDown);
  }, []);

  // Close the dropdown on outside clicks.
  useEffect(() => {
    function handleClick(e: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        setOpen(false);
        setMobileExpanded(false);
      }
    }
    document.addEventListener("mousedown", handleClick);
    return () => document.removeEventListener("mousedown", handleClick);
  }, []);

  // Close the dropdown on Escape.
  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") {
        setOpen(false);
        setMobileExpanded(false);
        inputRef.current?.blur();
      }
    }
    document.addEventListener("keydown", handleKeyDown);
    return () => document.removeEventListener("keydown", handleKeyDown);
  }, []);

  function handleSelect(entry: SearchEntry) {
    if (entry.kind === "job") {
      navigate(`/job/${encodeURIComponent(entry.job_id)}`);
    } else {
      navigate(`/job/${encodeURIComponent(entry.job_id)}/test/${encodeURIComponent(entry.test_name)}`);
    }
    setOpen(false);
    setQuery("");
    setMobileExpanded(false);
  }

  return (
    <Box ref={containerRef} sx={{ position: "relative", display: "flex", alignItems: "center" }}>
      <IconButton
        type="button"
        onClick={() => {
          setActivated(true);
          setMobileExpanded(true);
          setTimeout(() => inputRef.current?.focus(), 50);
        }}
        aria-label="Search"
        size="small"
        sx={{ display: { xs: "inline-flex", md: "none" }, color: "text.secondary" }}
      >
        <Search sx={{ fontSize: 20 }} />
      </IconButton>

      <Box
        sx={[
          mobileExpanded
            ? {
                position: "fixed",
                insetInline: 0,
                top: 0,
                zIndex: (theme) => theme.zIndex.modal,
                display: "flex",
                alignItems: "center",
                gap: 1,
                height: 64,
                px: 2,
                bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
                borderBottom: "1px solid",
                borderColor: "divider",
              }
            : { display: { xs: "none", md: "block" } },
        ]}
      >
        <Box sx={{ flex: 1, width: { xs: "100%", md: 256, lg: 320 } }}>
          <TextField
            inputRef={inputRef}
            type="text"
            value={query}
            onChange={(e) => {
              setActivated(true);
              setQuery(e.target.value);
              setOpen(true);
            }}
            onFocus={() => {
              setActivated(true);
              setOpen(true);
            }}
            placeholder="Search tests…"
            size="small"
            variant="outlined"
            fullWidth
            slotProps={{
              input: {
                startAdornment: (
                  <InputAdornment position="start">
                    <Search sx={{ fontSize: 18, color: "text.secondary" }} />
                  </InputAdornment>
                ),
                endAdornment: (
                  <InputAdornment position="end" sx={{ display: { xs: "none", sm: "flex" } }}>
                    <Box
                      component="kbd"
                      sx={{
                        pointerEvents: "none",
                        display: "inline-flex",
                        alignItems: "center",
                        border: "1px solid",
                        borderColor: "divider",
                        borderRadius: 1,
                        bgcolor: (theme) => (theme.vars ?? theme).palette.surface.main,
                        px: 0.75,
                        py: 0.25,
                        typography: "label",
                        fontSize: 10,
                        color: "text.secondary",
                      }}
                    >
                      ⌘K
                    </Box>
                  </InputAdornment>
                ),
              },
            }}
            sx={{
              "& .MuiOutlinedInput-root": {
                height: 36,
                borderRadius: "8px",
                bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
                color: "text.primary",
                fontSize: "0.875rem",
                "& fieldset": { borderColor: "divider" },
                "&:hover fieldset": { borderColor: "text.secondary" },
                "&.Mui-focused fieldset": { borderColor: "primary.main", borderWidth: 1 },
              },
              "& .MuiInputBase-input::placeholder": { color: "text.secondary", opacity: 0.6 },
            }}
          />
        </Box>
        {mobileExpanded && (
          <Button
            type="button"
            variant="text"
            onClick={() => {
              setMobileExpanded(false);
              setOpen(false);
              setQuery("");
            }}
            sx={{
              display: { xs: "inline-flex", md: "none" },
              flexShrink: 0,
              minWidth: 0,
              px: 0.5,
              color: "text.secondary",
              textTransform: "none",
              "&:hover": { color: "text.primary" },
            }}
          >
            Cancel
          </Button>
        )}
      </Box>

      {open && query.trim() && (
        <Panel
          elevation={8}
          sx={{
            position: mobileExpanded ? "fixed" : "absolute",
            top: mobileExpanded ? 64 : "calc(100% + 8px)",
            left: mobileExpanded ? 16 : { md: 0 },
            right: mobileExpanded ? 16 : { xs: 0, md: "auto" },
            width: mobileExpanded ? "auto" : { xs: "min(28rem, calc(100vw - 32px))", md: "28rem" },
            borderRadius: "8px",
            overflow: "hidden",
            zIndex: (theme) => theme.zIndex.modal,
          }}
        >
          <Box sx={{ maxHeight: 400, overflowY: "auto" }}>
            {loading ? (
              <Box sx={{ px: 4, py: 3, textAlign: "center" }}>
                <Typography variant="body2" color="text.secondary" role="status">
                  Loading search index...
                </Typography>
              </Box>
            ) : error ? (
              <Box sx={{ px: 4, py: 3, textAlign: "center" }}>
                <Typography variant="body2" color="text.secondary">
                  Search is temporarily unavailable.
                </Typography>
              </Box>
            ) : results.length === 0 ? (
              <Box sx={{ px: 4, py: 3, textAlign: "center" }}>
                <Typography variant="body2" color="text.secondary">
                  No results
                </Typography>
              </Box>
            ) : (
              <List disablePadding dense>
                {Array.from(grouped.entries()).map(([jobID, group]) => (
                  <Box key={jobID} component="li" sx={{ listStyle: "none" }}>
                    <ListSubheader
                      component="div"
                      sx={{
                        position: "sticky",
                        top: 0,
                        zIndex: 1,
                        bgcolor: (theme) => (theme.vars ?? theme).palette.surface.container,
                        borderBottom: "1px solid",
                        borderColor: "divider",
                        px: 1.5,
                        py: 0.75,
                        lineHeight: 1.4,
                        typography: "label",
                        fontSize: 12,
                        fontWeight: 600,
                        color: "text.secondary",
                      }}
                    >
                      {shortJobName(group.jobName, filePrefix)}
                    </ListSubheader>
                    {group.items.map((r) => (
                      <ListItemButton
                        key={`${r.item.kind}:${r.item.job_id}/${r.item.test_name}`}
                        component="button"
                        type="button"
                        onClick={() => handleSelect(r.item)}
                        sx={{
                          width: "100%",
                          display: "flex",
                          alignItems: "center",
                          gap: 1.5,
                          px: 1.5,
                          py: 1,
                          color: "text.primary",
                          textAlign: "left",
                          transition: "background-color 150ms ease",
                          "&:hover": { bgcolor: (theme) => (theme.vars ?? theme).palette.surface.containerHigh },
                        }}
                      >
                        {r.item.kind === "job" ? (
                          <>
                            <Box
                              sx={{
                                width: 8,
                                height: 8,
                                borderRadius: "50%",
                                flexShrink: 0,
                                bgcolor: "primary.main",
                              }}
                            />
                            <Typography variant="body2" noWrap sx={{ minWidth: 0, flex: 1, fontWeight: 600 }}>
                              {r.item.tab_name || shortJobName(r.item.job_name, filePrefix)}
                            </Typography>
                            <Typography
                              variant="label"
                              noWrap
                              sx={{ flexShrink: 0, fontSize: 12, color: "text.secondary" }}
                            >
                              {r.item.branch}
                            </Typography>
                          </>
                        ) : (
                          <>
                            <Box
                              sx={{
                                width: 8,
                                height: 8,
                                borderRadius: "50%",
                                flexShrink: 0,
                                bgcolor: r.item.status === "passed" ? "success.main" : "error.main",
                              }}
                            />
                            <Typography variant="body2" noWrap sx={{ minWidth: 0, flex: 1 }}>
                              {shortTestName(r.item.test_name)}
                            </Typography>
                            {r.item.fail_rate > 0 && (
                              <Chip
                                size="small"
                                color="error"
                                label={`${Math.round(r.item.fail_rate * 100)}%`}
                                sx={{
                                  flexShrink: 0,
                                  height: 22,
                                  bgcolor: (theme) => soft(theme, "error", 0.18),
                                  color: "error.main",
                                  fontWeight: 600,
                                  "& .MuiChip-label": { px: 1 },
                                }}
                              />
                            )}
                          </>
                        )}
                      </ListItemButton>
                    ))}
                  </Box>
                ))}
              </List>
            )}
          </Box>
        </Panel>
      )}
    </Box>
  );
}
