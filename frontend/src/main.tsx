import { createRoot } from "react-dom/client";
import { CssBaseline } from "@mui/material";
import { ThemeProvider } from "@mui/material/styles";
import { DrivePage } from "./pages/DrivePage";
import { theme } from "./ui/sharedTheme";
import { createThemeStorageManager, MODE_STORAGE_KEY } from "./ui/themeStorage";

const storageManager = createThemeStorageManager("drive-color-mode");

createRoot(document.getElementById("root")!).render(
  <ThemeProvider
    theme={theme}
    defaultMode="system"
    modeStorageKey={MODE_STORAGE_KEY}
    storageManager={storageManager}
    disableTransitionOnChange
  >
    <CssBaseline enableColorScheme />
    <DrivePage />
  </ThemeProvider>,
);
