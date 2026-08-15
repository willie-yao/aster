import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Typography from "@mui/material/Typography";
import { Link as RouterLink } from "react-router-dom";

export function NotFoundPage() {
  return (
    <Box sx={{ maxWidth: 720, mx: "auto", py: { xs: 4, sm: 8 } }}>
      <Typography variant="h4" component="h1">
        Page not found
      </Typography>
      <Typography color="text.secondary" sx={{ mt: 1.5 }}>
        The requested dashboard page does not exist.
      </Typography>
      <Link component={RouterLink} to="/" sx={{ display: "inline-block", mt: 2 }}>
        Overview
      </Link>
    </Box>
  );
}
