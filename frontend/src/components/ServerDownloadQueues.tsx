import { useEffect, useState } from "react";
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Alert,
  Box,
  Button,
  Checkbox,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  Stack,
  Tab,
  Tabs,
  TextField,
  Typography,
} from "@mui/material";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import type { QueueSummary } from "../serverDownloadQueues";
import { CurrentDownloadQueue } from "./CurrentDownloadQueue";
import { useServerDownloadQueues } from "./useServerDownloadQueues";

interface ServerDownloadQueuesProps {
  path: string;
  canSubmit: boolean;
  accessAllowed: boolean;
  accessDenied: boolean;
}
const knownQueueStates = new Set([
  "discovering",
  "queued",
  "running",
  "paused",
  "cancelling",
  "cancelled",
  "complete",
  "partial",
  "expired",
  "needs_verification",
  "blocked",
]);
function stateLabel(state: string, known: Set<string>) {
  return known.has(state)
    ? state
    : `Unknown state: ${state} (not confirmed complete)`;
}
function QueueCounts({ queue }: { queue: QueueSummary }) {
  return (
    <Box sx={{ overflowWrap: "anywhere" }}>
      <Typography>
        {stateLabel(queue.state, knownQueueStates)} · /{queue.path}
      </Typography>
      <Typography variant="body2">
        {queue.completedItems}/{queue.totalItems} complete · {queue.queuedItems}{" "}
        queued · {queue.activeItems} active · {queue.savingItems} saving ·{" "}
        {queue.failedItems} failed · {queue.cancelledItems} cancelled ·{" "}
        {queue.needsVerificationItems} needs_verification
      </Typography>
      <Typography variant="body2">
        {queue.completedBytes} / {queue.totalBytes} known bytes ·{" "}
        {queue.unknownSizeItems} unknown sizes · discovery:{" "}
        {queue.discoveryState ?? "not reported"}
      </Typography>
      {queue.errorCode && (
        <Alert severity="warning">
          Source/queue error: {queue.errorCode}. Failed sources do not stop
          other items.
        </Alert>
      )}
      {!!queue.normalizedNames && (
        <Alert severity="warning">
          {queue.normalizedNames} names normalized (edge whitespace trimmed).
          Collisions are conflicts, never merged or overwritten.
        </Alert>
      )}
      {queue.needsVerificationItems > 0 && (
        <Alert severity="warning">
          Publication uncertain: verify the destination. needs_verification is
          not a blind download retry; use a different target/new intent if
          needed.
        </Alert>
      )}
    </Box>
  );
}
function Paging({
  offset,
  total,
  disabled,
  onChange,
  label,
}: {
  offset: number;
  total: number;
  disabled: boolean;
  onChange: (offset: number) => void;
  label: string;
}) {
  return (
    <Stack direction="row" spacing={1} sx={{ alignItems: "center" }}>
      <Button
        aria-label={`Previous ${label}`}
        disabled={disabled || offset === 0}
        onClick={() => onChange(Math.max(0, offset - 100))}
      >
        Previous
      </Button>
      <Typography variant="body2">
        {total === 0 ? 0 : offset + 1}–{Math.min(offset + 100, total)} of{" "}
        {total}
      </Typography>
      <Button
        aria-label={`Next ${label}`}
        disabled={disabled || offset + 100 >= total}
        onClick={() => onChange(offset + 100)}
      >
        Next
      </Button>
    </Stack>
  );
}

export function ServerDownloadQueues({
  path,
  canSubmit,
  accessAllowed,
  accessDenied,
}: ServerDownloadQueuesProps) {
  const [expanded, setExpanded] = useState(false);
  const [mode, setMode] = useState<"direct" | "yandex">("direct");
  const [draft, setDraft] = useState("");
  const [skipVerified, setSkipVerified] = useState(true);
  const [validation, setValidation] = useState("");
  const [forgetOpen, setForgetOpen] = useState(false);
  const queues = useServerDownloadQueues(
    expanded && accessAllowed,
    accessDenied,
  );
  const disabled = !accessAllowed || queues.authStopped || queues.busy;
  useEffect(() => {
    if (queues.draftReset) {
      setDraft("");
      setValidation("");
      setForgetOpen(false);
    }
  }, [queues.draftReset]);
  useEffect(() => {
    if (queues.authStopped) {
      setMode("direct");
      setSkipVerified(true);
    }
  }, [queues.authStopped]);

  function submit() {
    if (!canSubmit || disabled) return;
    const urls = draft
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter(Boolean);
    if (
      !urls.length ||
      urls.length > 1000 ||
      (mode === "yandex" && urls.length !== 1)
    ) {
      setValidation(
        "Enter 1–1000 public URLs, or one stable Yandex public file/folder link.",
      );
      return;
    }
    try {
      for (const value of urls) {
        const url = new URL(value);
        if (
          !["http:", "https:"].includes(url.protocol) ||
          url.username ||
          url.password ||
          (mode === "yandex" && url.protocol !== "https:")
        )
          throw new Error();
      }
    } catch {
      setValidation(
        "Use public HTTP(S) URLs without credentials; Yandex links require HTTPS. The server validates public destinations and supported share hosts.",
      );
      return;
    }
    setValidation("");
    void queues.submit({
      path,
      space: "personal",
      existing: skipVerified ? "skip_verified" : "error",
      sources: urls.map((url) => ({ type: mode, url })),
    });
  }

  if (!accessAllowed)
    return (
      <Accordion
        expanded={expanded}
        onChange={(_, value) => setExpanded(value)}
      >
        <AccordionSummary expandIcon={<ExpandMoreIcon />}>
          <Typography>Server download queues</Typography>
        </AccordionSummary>
        <AccordionDetails>
          <Alert severity="warning">
            Drive access is unavailable. Queue controls and polling stopped;
            accepted server work continues. Retry Drive access or sign in with
            the queue owner.
          </Alert>
        </AccordionDetails>
      </Accordion>
    );

  return (
    <Accordion expanded={expanded} onChange={(_, value) => setExpanded(value)}>
      <AccordionSummary expandIcon={<ExpandMoreIcon />}>
        <Typography>Server download queues</Typography>
      </AccordionSummary>
      <AccordionDetails>
        <Stack spacing={2}>
          <Alert severity="info">
            After durable server acceptance, downloads continue even if you
            close the browser/PC, log out, or your session expires. Reload to
            see server queues; source drafts are never saved in browser storage.
            Pause/cancel before revoking Drive access if work must stop.
          </Alert>
          <Typography variant="body2">
            Server limits: 1000 sources per submission, 10,000 retained items
            per owner / 50,000 per process; two active downloads per owner /
            four per server (shared with single-URL imports), 10 GiB per file,
            20 GiB staging and 2 GiB reserve. Queues expire after seven days;
            execution deadline is two hours from first start. Failed sources do
            not stop others.
          </Typography>
          {!canSubmit && (
            <Alert severity="info">
              New queues require the current personal directory. Your existing
              owner queues can still be managed from any browsing space.
            </Alert>
          )}
          <Typography>Destination: personal /{path}</Typography>
          <Tabs
            value={mode}
            onChange={(_, value: "direct" | "yandex") => setMode(value)}
            aria-label="Queue source mode"
          >
            <Tab
              value="direct"
              label="Bulk public URLs"
              disabled={disabled || !!queues.pending}
            />
            <Tab
              value="yandex"
              label="Yandex public link"
              disabled={disabled || !!queues.pending}
            />
          </Tabs>
          <TextField
            label={
              mode === "direct"
                ? "Public URLs (one per line)"
                : "Stable Yandex public file/folder link"
            }
            multiline
            minRows={3}
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            disabled={disabled || !canSubmit || !!queues.pending}
          />
          <FormControlLabel
            control={
              <Checkbox
                checked={skipVerified}
                onChange={(_, value) => setSkipVerified(value)}
                disabled={disabled || !!queues.pending}
              />
            }
            label="Skip existing only after exact size + expected SHA-256 verification; missing SHA or mismatch is a conflict, never overwrite"
          />
          <Button
            variant="contained"
            disabled={disabled || !canSubmit || !draft.trim()}
            onClick={submit}
          >
            {queues.pending
              ? "Retry unchanged submission (check receipt first)"
              : "Submit server queue"}
          </Button>
          {queues.pending && (
            <Alert
              severity="warning"
              action={
                <Button
                  disabled={disabled}
                  onClick={() => void queues.reconcile()}
                >
                  Check submission receipt
                </Button>
              }
            >
              Acceptance not yet confirmed. Do not create a replacement intent.
              Only an opaque submission ID is saved for receipt recovery; no
              URLs or request bodies.
            </Alert>
          )}
          {queues.pending && (
            <>
              <Alert severity="warning">
                The original queue may still run. Check server queues before a
                new intent. Forgetting receipt tracking only removes local
                tracking and drafts; it does not cancel any server work.
              </Alert>
              <Button onClick={() => setForgetOpen(true)}>
                Forget receipt tracking
              </Button>
              <Dialog
                open={forgetOpen}
                onClose={() => setForgetOpen(false)}
                aria-labelledby="forget-receipt-title"
              >
                <DialogTitle id="forget-receipt-title">
                  Forget local receipt tracking?
                </DialogTitle>
                <DialogContent>
                  The original queue may still run. Check server queues before
                  submitting a new intent, which could duplicate work. This
                  removes only the local opaque receipt ID and draft metadata.
                  It does not cancel or pause server work.
                </DialogContent>
                <DialogActions>
                  <Button onClick={() => setForgetOpen(false)}>
                    Keep tracking
                  </Button>
                  <Button
                    color="warning"
                    onClick={() => {
                      queues.forget();
                      setMode("direct");
                      setSkipVerified(true);
                      setForgetOpen(false);
                    }}
                  >
                    Confirm forget tracking
                  </Button>
                </DialogActions>
              </Dialog>
            </>
          )}
          {validation && <Alert severity="error">{validation}</Alert>}
          {queues.removalNotice && (
            <Alert severity="info">{queues.removalNotice}</Alert>
          )}
          {queues.error && <Alert severity="error">{queues.error}</Alert>}
          <Button disabled={disabled} onClick={() => void queues.refresh()}>
            Refresh server queues
          </Button>
          {!queues.page && !queues.error && (
            <Typography>Loading server queues…</Typography>
          )}
          {queues.page && (
            <>
              {queues.page.queues.length === 0 && (
                <Typography>No server queues.</Typography>
              )}
              {queues.page.queues.map((queue) => (
                <Box
                  key={queue.id}
                  sx={{
                    border: 1,
                    borderColor: "divider",
                    borderRadius: 1,
                    p: 2,
                  }}
                >
                  <Typography variant="subtitle2">Queue {queue.id}</Typography>
                  <QueueCounts queue={queue} />
                  <Button
                    disabled={disabled}
                    onClick={() => queues.select(queue.id)}
                    aria-label={`Details ${queue.id}`}
                  >
                    Details
                  </Button>
                </Box>
              ))}
              <Paging
                offset={queues.offset}
                total={queues.page.total}
                disabled={disabled}
                onChange={queues.setOffset}
                label="queues"
              />
            </>
          )}
          {queues.selected && (
            <Box sx={{ borderTop: 1, borderColor: "divider", pt: 2 }}>
              <Typography variant="h6">
                Queue details: {queues.selected}
              </Typography>
              {queues.detail ? (
                <Stack spacing={1}>
                  <QueueCounts queue={queues.detail} />
                  <Typography variant="body2">
                    Accepted: {queues.detail.acceptedAt} · Expires:{" "}
                    {queues.detail.expiresAt}
                  </Typography>
                  <Typography variant="body2">
                    Pause stops new work and lets active downloads/saves finish.
                    Cancel stops new work; saving files may finish. No rollback
                    or destination deletion.
                  </Typography>
                  <Stack direction="row" spacing={1}>
                    <Button
                      disabled={
                        disabled ||
                        !["discovering", "queued", "running"].includes(
                          queues.detail.state,
                        )
                      }
                      onClick={() =>
                        void queues.control(queues.selected!, "pause")
                      }
                    >
                      Pause queue
                    </Button>
                    <Button
                      disabled={disabled || queues.detail.state !== "paused"}
                      onClick={() =>
                        void queues.control(queues.selected!, "resume")
                      }
                    >
                      Resume queue
                    </Button>
                    <Button
                      disabled={
                        disabled ||
                        ![
                          "discovering",
                          "queued",
                          "running",
                          "paused",
                          "blocked",
                          "needs_verification",
                        ].includes(queues.detail.state)
                      }
                      onClick={() =>
                        void queues.control(queues.selected!, "cancel")
                      }
                    >
                      Cancel queue (saving may finish)
                    </Button>
                  </Stack>
                </Stack>
              ) : (
                <Typography>Loading queue details…</Typography>
              )}
              {queues.items && (
                <Stack spacing={1}>
                  {queues.items.items.length === 0 && (
                    <Typography>
                      No items on this page; discovery may still be running.
                    </Typography>
                  )}
                  <CurrentDownloadQueue
                    key={`${queues.selected}:${queues.itemOffset}:${queues.draftReset}`}
                    page={queues.items}
                    disabled={disabled}
                    onRemove={(ids) =>
                      queues.removeItems(queues.selected!, ids)
                    }
                    onRetry={(id) =>
                      queues.itemControl(queues.selected!, id, "retry")
                    }
                  />
                  <Paging
                    offset={queues.itemOffset}
                    total={queues.items.total}
                    disabled={disabled}
                    onChange={queues.setItemOffset}
                    label="items"
                  />
                </Stack>
              )}
            </Box>
          )}
        </Stack>
      </AccordionDetails>
    </Accordion>
  );
}
