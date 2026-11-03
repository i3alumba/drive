import { createRoot } from "react-dom/client";
import { CssBaseline } from "@mui/material";
import { ThemeProvider } from "@mui/material/styles";
import { DrivePage } from "./pages/DrivePage";
import { PublicDownload } from "./pages/PublicDownload";
import { theme } from "./ui/sharedTheme";
import { createThemeStorageManager, MODE_STORAGE_KEY } from "./ui/themeStorage";

const publicLanding = window.location.pathname.startsWith("/public/");
const publicRoute = /^\/public\/([1-9][0-9]*)\/([a-f0-9]{64})$/.exec(
  window.location.pathname,
);
const publicToken = publicLanding ? window.location.hash.slice(1) : "";
if (publicLanding)
  window.history.replaceState(null, "", window.location.pathname);
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
    {publicLanding ? (
      <PublicDownload
        owner={publicRoute?.[1] ?? ""}
        id={publicRoute?.[2] ?? ""}
        token={publicToken}
      />
    ) : (
      <DrivePage />
    )}
  </ThemeProvider>,
);
