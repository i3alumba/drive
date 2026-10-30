import { IconButton, Tooltip } from "@mui/material";
import { useColorScheme } from "@mui/material/styles";
import DarkModeIcon from "@mui/icons-material/DarkMode";
import LightModeIcon from "@mui/icons-material/LightMode";

export function ThemeToggle() {
  const { mode, systemMode, setMode } = useColorScheme();
  const resolvedMode = mode === "system" ? systemMode : mode;
  if (!resolvedMode) return null;

  const dark = resolvedMode === "dark";
  const label = dark ? "Switch to light theme" : "Switch to dark theme";

  return (
    <Tooltip title={label}>
      <IconButton
        color="inherit"
        aria-label={label}
        onClick={() => setMode(dark ? "light" : "dark")}
      >
        {dark ? <LightModeIcon /> : <DarkModeIcon />}
      </IconButton>
    </Tooltip>
  );
}
