import { useEffect, useRef, useState } from "react";
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  TextField,
  Typography,
} from "@mui/material";
import { apiFetch } from "../api";
import type { DriveObject } from "../types";
type PublicLink = {
  id: string;
  path: string;
  expiresAt: string;
  cleanupOnly?: boolean;
};
export function PublicLinkDialog({
  item,
  open,
  onClose,
}: {
  item: DriveObject | null;
  open: boolean;
  onClose: () => void;
}) {
  const [links, setLinks] = useState<PublicLink[]>([]),
    [url, setUrl] = useState(""),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false);
  const generation = useRef(0);
  useEffect(() => {
    generation.current++;
    let active = true;
    setUrl("");
    setError("");
    setLinks([]);
    setBusy(open);
    if (open)
      apiFetch(
        `/api/public-links${item ? `?path=${encodeURIComponent(item.path)}` : ""}`,
      )
        .then(async (response) => {
          if (!response.ok) throw new Error();
          const value = await response.json();
          if (active) setLinks(value);
        })
        .catch(() => {
          if (active) setError("Could not load public links.");
        })
        .finally(() => {
          if (active) setBusy(false);
        });
    return () => {
      active = false;
      generation.current++;
    };
  }, [item, open]);
  async function create() {
    if (!item) return;
    const opened = generation.current;
    setBusy(true);
    setError("");
    try {
      const response = await apiFetch("/api/public-links", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: item.path, days: 7 }),
      });
      if (!response.ok) throw new Error();
      const value = await response.json();
      if (opened !== generation.current) return;
      setUrl(new URL(value.downloadUrl, window.location.origin).href);
      setLinks((current) => [...current, value.link]);
    } catch {
      if (opened === generation.current)
        setError("Could not create public link.");
    } finally {
      if (opened === generation.current) setBusy(false);
    }
  }
  async function revoke(id: string) {
    const opened = generation.current;
    setBusy(true);
    setError("");
    try {
      const response = await apiFetch(
        `/api/public-links/${encodeURIComponent(id)}`,
        { method: "DELETE" },
      );
      if (!response.ok) throw new Error();
      if (opened !== generation.current) return;
      setLinks((current) => current.filter((link) => link.id !== id));
      setUrl("");
    } catch {
      if (opened === generation.current) setError("Could not revoke link.");
    } finally {
      if (opened === generation.current) setBusy(false);
    }
  }
  return (
    <Dialog open={open} onClose={busy ? undefined : onClose} fullWidth>
      <DialogTitle>
        {item ? `Public link: ${item.name}` : "Manage public links"}
      </DialogTitle>
      <DialogContent>
        <Stack spacing={2}>
          <Alert severity="warning">
            Anyone with the link can download a read-only snapshot without
            signing in. Valid for 7 days. Creating a link stores a separate
            copy; later file changes are not shared. Copy the URL before
            closing: it is shown only once. Deleting the original does not
            revoke the link. Revoke old links to remove their stored copies.
          </Alert>
          {error && <Alert severity="error">{error}</Alert>}
          {url && (
            <>
              <TextField
                label="Public download URL"
                value={url}
                slotProps={{ input: { readOnly: true } }}
              />
              <Button
                onClick={() =>
                  navigator.clipboard
                    .writeText(url)
                    .catch(() =>
                      setError(
                        "Copy failed. Select and copy the link manually.",
                      ),
                    )
                }
              >
                Copy link
              </Button>
            </>
          )}
          {links.map((link) => (
            <Stack
              key={link.id}
              direction="row"
              sx={{ alignItems: "center", justifyContent: "space-between" }}
            >
              <Typography variant="body2">
                {link.cleanupOnly
                  ? "Unlinked snapshot — cleanup required"
                  : `${link.path}: expires ${new Date(link.expiresAt).toLocaleString()}`}
              </Typography>
              <Button disabled={busy} onClick={() => void revoke(link.id)}>
                Revoke link
              </Button>
            </Stack>
          ))}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button disabled={busy} onClick={onClose}>
          Close
        </Button>
        {item && (
          <Button disabled={busy} onClick={() => void create()}>
            Create public link
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}
