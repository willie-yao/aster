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

// ProfileMenu is the navbar account control. It appears only in oauth mode:
// a "Sign in" button when signed out, or an account menu with the login and a
// sign-out action when signed in. Proxy and static modes render nothing.
export function ProfileMenu() {
  const { status, login, mode, signIn, signOut } = useAuth();
  const { engine } = useCapabilities();
  const [anchor, setAnchor] = useState<null | HTMLElement>(null);

  if (mode !== "oauth" || status === "loading" || status === "unavailable") {
    return engine ? (
      <Typography variant="caption" color="text.secondary" title={`Engine ${engine.commit} (${engine.image_tag})`}>
        Engine {engine.commit === "dev" ? "dev" : engine.commit.slice(0, 7)}
      </Typography>
    ) : null;
  }

  if (status === "anonymous") {
    return (
      <Button
        size="small"
        startIcon={<GitHub sx={{ fontSize: 18 }} />}
        onClick={signIn}
        sx={{
          color: "text.secondary",
          textTransform: "none",
          whiteSpace: "nowrap",
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
        sx={{ color: "text.secondary", "&:hover": { color: "text.primary" } }}
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
          <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
            Signed in as
          </Typography>
          <Typography variant="body2" sx={{ fontWeight: 600 }}>
            {login}
          </Typography>
        </Box>
        {engine && (
          <Box sx={{ px: 2, pb: 1 }}>
            <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>Engine</Typography>
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
