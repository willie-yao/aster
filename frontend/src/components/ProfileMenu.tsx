import { useState } from "react";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import ButtonBase from "@mui/material/ButtonBase";
import Divider from "@mui/material/Divider";
import IconButton from "@mui/material/IconButton";
import ListItemIcon from "@mui/material/ListItemIcon";
import Menu from "@mui/material/Menu";
import MenuItem from "@mui/material/MenuItem";
import Typography from "@mui/material/Typography";
import { AccountCircle, GitHub, Logout } from "@mui/icons-material";
import { useAuth } from "../hooks/useAuth";
import { useCapabilities } from "../hooks/useCapabilities";
import { overlayPaperSx } from "../theme/overview";

// ProfileMenu is the account control. It appears only in oauth mode: a
// "Sign in" button when signed out, or an account menu with the login and a
// sign-out action when signed in. Proxy and static modes render nothing.
// `compact` stacks the glyph over the label to match the navigation rail's
// destination items, so the control reads as an action rather than a link to
// the project's GitHub page.
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
        sx={compact ? { fontSize: "0.6875rem", maxWidth: "100%", px: 0.5, overflow: "hidden", textOverflow: "ellipsis" } : undefined}
      >
        {compact ? commit : `Engine ${commit}`}
      </Typography>
    );
  }

  if (status === "anonymous") {
    if (compact) {
      return (
        <ButtonBase
          aria-label="Sign in with GitHub"
          title="Sign in with GitHub"
          onClick={signIn}
          sx={{
            width: "100%",
            height: 54,
            display: "flex",
            flexDirection: "column",
            alignItems: "center",
            justifyContent: "center",
            gap: 0.375,
            color: "text.secondary",
            transition: "color 140ms ease, background-color 140ms ease",
            "@media (prefers-reduced-motion: reduce)": { transition: "none" },
            "&:hover": { color: "text.primary", bgcolor: "surface.containerHigh" },
            "&.Mui-focusVisible": {
              outline: "2px solid",
              outlineColor: "primary.main",
              outlineOffset: -2,
            },
          }}
        >
          <GitHub aria-hidden="true" sx={{ fontSize: 20 }} />
          <Typography
            component="span"
            sx={{ fontSize: "0.6875rem", fontWeight: 600, lineHeight: 1.2, letterSpacing: "0.01em" }}
          >
            Sign in
          </Typography>
        </ButtonBase>
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
        slotProps={{ paper: { sx: overlayPaperSx } }}
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
