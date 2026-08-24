import { useState } from "react";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Divider from "@mui/material/Divider";
import IconButton from "@mui/material/IconButton";
import ListItemIcon from "@mui/material/ListItemIcon";
import Menu from "@mui/material/Menu";
import MenuItem from "@mui/material/MenuItem";
import Typography from "@mui/material/Typography";
import { AccountCircle, GitHub, Logout } from "@mui/icons-material";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";

// ProfileMenu is the account control. It appears only in oauth mode: a
// "Sign in" button when signed out, or an account menu with the login and a
// sign-out action when signed in. Proxy and static modes render nothing.
// `compact` renders icon-only for the navigation rail, whose 76px column
// cannot fit the labelled control.
export function ProfileMenu({ compact = false }: { compact?: boolean } = {}) {
  const { status, login, mode, signIn, signOut } = useAuth();
  const { engine } = useCapabilities();
  const [anchor, setAnchor] = useState<null | HTMLElement>(null);

  if (mode !== "oauth" || status === "loading" || status === "unavailable") {
    if (!engine) return null;
    const commit = engine.commit === "dev" ? "dev" : engine.commit.slice(0, 7);
    return (
      <Typography
        variant="caption"
        color="textSecondary"
        title={`Engine ${engine.commit} (${engine.image_tag})`}
        sx={compact ? { fontSize: "0.5625rem", maxWidth: "100%", px: 0.5, overflow: "hidden", textOverflow: "ellipsis" } : undefined}
      >
        {compact ? commit : `Engine ${commit}`}
      </Typography>
    );
  }

  if (status === "anonymous") {
    if (compact) {
      return (
        <IconButton
          aria-label="Sign in"
          size="small"
          onClick={signIn}
          sx={{
            width: 44,
            height: 44,
            color: "text.secondary",
            "&:hover": { color: "text.primary" },
          }}
        >
          <GitHub sx={{ fontSize: 18 }} />
        </IconButton>
      );
    }
    return (
      <Button
        size="small"
        startIcon={<GitHub sx={{ fontSize: 18 }} />}
        onClick={signIn}
        sx={{
          color: "text.secondary",
          textTransform: "none",
          whiteSpace: "nowrap",
          minHeight: { xs: 44, sm: 36 },
          "&:hover": { color: "text.primary" },
        }}
      >
        Sign in
      </Button>
    );
  }

  return (
    <>
      <IconButton
        aria-label="Account"
        size="small"
        onClick={(e) => setAnchor(e.currentTarget)}
        sx={{
          width: { xs: 44, sm: 36 },
          height: { xs: 44, sm: 36 },
          color: "text.secondary",
          "&:hover": { color: "text.primary" },
        }}
      >
        <AccountCircle fontSize="small" />
      </IconButton>
      <Menu
        anchorEl={anchor}
        open={Boolean(anchor)}
        onClose={() => setAnchor(null)}
        anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
        transformOrigin={{ vertical: "top", horizontal: "right" }}
      >
        <Box sx={{ px: 2, py: 1 }}>
          <Typography variant="caption" color="textSecondary" sx={{ display: "block" }}>
            Signed in as
          </Typography>
          <Typography variant="body2" sx={{ fontWeight: 600 }}>
            {login}
          </Typography>
        </Box>
        {engine && (
          <Box sx={{ px: 2, pb: 1 }}>
            <Typography variant="caption" color="textSecondary" sx={{ display: "block" }}>Engine</Typography>
            <Typography variant="caption" sx={{ fontFamily: "monospace" }}>{engine.commit} · {engine.image_tag}</Typography>
          </Box>
        )}
        <Divider />
        <MenuItem
          onClick={() => {
            setAnchor(null);
            void signOut();
          }}
        >
          <ListItemIcon>
            <Logout fontSize="small" />
          </ListItemIcon>
          Sign out
        </MenuItem>
      </Menu>
    </>
  );
}
