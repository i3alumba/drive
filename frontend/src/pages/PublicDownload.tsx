import { useEffect, useState } from "react";
import {
  Alert,
  Button,
  Container,
  Paper,
  Stack,
  Typography,
} from "@mui/material";

// This page deliberately does not mount DrivePage or read/send a portal bearer.
// The fragment is consumed in main.tsx before rendering/network requests.
export function PublicDownload({
  owner,
  id,
  token,
}: {
  owner: string;
  id: string;
  token: string;
}) {
  const [capability, setCapability] = useState(token);
  useEffect(() => {
    const consume = () => {
      setCapability(window.location.hash.slice(1));
      window.history.replaceState(null, "", window.location.pathname);
      setError("");
    };
    window.addEventListener("hashchange", consume);
    return () => window.removeEventListener("hashchange", consume);
  }, []);
  const [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  const valid =
    /^[1-9][0-9]*$/.test(owner) &&
    /^[a-f0-9]{64}$/.test(id) &&
    /^[a-f0-9]{64}$/.test(capability);
  async function download() {
    setBusy(true);
    setError("");
    try {
      const response = await fetch(`/api/public/${owner}/${id}/session`, {
        method: "POST",
        credentials: "same-origin",
        referrerPolicy: "no-referrer",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token: capability }),
      });
      if (!response.ok) throw new Error();
      // Native browser download streams to disk; never buffer the file as a JS Blob.
      window.location.assign(`/api/public/${owner}/${id}`);
    } catch {
      setError(
        "Link expired, revoked or unavailable. Open the original shared link again.",
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <Container maxWidth="sm" sx={{ py: 6 }}>
      <Paper sx={{ p: 3 }}>
        <Stack spacing={2}>
          <Typography component="h1" variant="h5">
            Shared Drive file
          </Typography>
          <Typography>
            Download a read-only snapshot. No account is required.
          </Typography>
          {!valid && (
            <Alert severity="error">
              Open the original complete shared link, including its fragment.
            </Alert>
          )}
          {error && <Alert severity="error">{error}</Alert>}
          <Button
            variant="contained"
            disabled={!valid || busy}
            onClick={() => void download()}
          >
            {busy ? "Preparing download…" : "Download file"}
          </Button>
        </Stack>
      </Paper>
    </Container>
  );
}
