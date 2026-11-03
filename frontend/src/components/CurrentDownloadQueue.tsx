import { useEffect, useState } from "react";
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Stack,
  Tab,
  Tabs,
  Typography,
} from "@mui/material";
import {
  canRemoveQueueItem,
  type QueueItemPage,
} from "../serverDownloadQueues";
const knownItemStates = new Set([
  "queued",
  "resolving",
  "downloading",
  "saving",
  "retry_wait",
  "complete",
  "failed",
  "cancelled",
  "expired",
  "needs_verification",
]);

function stateLabel(state: string, known: Set<string>) {
  return known.has(state)
    ? state
    : `Unknown state: ${state} (not confirmed complete)`;
}
const historyStates = new Set([
  "complete",
  "failed",
  "cancelled",
  "expired",
  "needs_verification",
]);
interface CurrentDownloadQueueProps {
  page: QueueItemPage;
  disabled: boolean;
  onRemove: (ids: string[]) => Promise<void>;
  onRetry: (id: string) => Promise<void>;
}
export function CurrentDownloadQueue({
  page,
  disabled,
  onRemove,
  onRetry,
}: CurrentDownloadQueueProps) {
  const [view, setView] = useState("current");
  const [selected, setSelected] = useState<string[]>([]);
  const visible = page.items.filter(
    (item) => historyStates.has(item.state) === (view === "history"),
  );
  // Polling can advance an item to saving or history: never retain its selection.
  useEffect(() => {
    setSelected((ids) =>
      ids.filter((id) =>
        visible.some((item) => item.id === id && canRemoveQueueItem(item)),
      ),
    );
  }, [page, view]);
  return (
    <Stack spacing={1}>
      <Typography variant="h6">Queue links</Typography>
      <Typography variant="body2">
        Remove requests cancellation of eligible work only. Durable receipts
        stay in History; no destination files are deleted. Saving or already
        published data cannot be rolled back.
      </Typography>
      <Tabs
        value={view}
        onChange={(_, value: string) => {
          setView(value);
          setSelected([]);
        }}
        aria-label="Queue item view"
      >
        <Tab value="current" label="Current queue" disabled={disabled} />
        <Tab value="history" label="History" disabled={disabled} />
      </Tabs>
      <Typography variant="body2">
        Filter and selection apply only to this loaded page (up to 100
        receipts), not unloaded pages. {selected.length} selected.
      </Typography>
      <Stack direction="row">
        <Button
          disabled={disabled || !visible.some(canRemoveQueueItem)}
          onClick={() =>
            setSelected(
              visible.filter(canRemoveQueueItem).map((item) => item.id),
            )
          }
        >
          Select removable on this page
        </Button>
        <Button
          disabled={disabled || !selected.length}
          onClick={() => setSelected([])}
        >
          Clear selection
        </Button>
        <Button
          disabled={disabled || !selected.length}
          onClick={() => {
            const ids = selected;
            setSelected([]);
            void onRemove(ids);
          }}
        >
          Remove selected
        </Button>
      </Stack>
      {!visible.length && (
        <Typography>
          No {view === "current" ? "current queue" : "history"} items on this
          loaded page. Other pages may contain matching items.
        </Typography>
      )}
      {visible.map((item) => (
        <Box key={item.id} sx={{ p: 1, overflowWrap: "anywhere" }}>
          <Checkbox
            slotProps={{ input: { "aria-label": `Select item ${item.name}` } }}
            disabled={disabled || !canRemoveQueueItem(item)}
            checked={selected.includes(item.id)}
            onChange={(_, checked) =>
              setSelected((ids) =>
                checked
                  ? [...ids, item.id]
                  : ids.filter((id) => id !== item.id),
              )
            }
          />
          <Typography>
            {item.position}. {item.name} ·{" "}
            {stateLabel(item.state, knownItemStates)}
          </Typography>
          <Typography variant="body2">
            /{item.path} · {item.bytes} / {item.total || "unknown"} bytes ·
            attempts {item.attempts} (total{" "}
            {item.totalAttempts ?? item.attempts})
            {item.reused ? " · verified existing file" : ""}
          </Typography>
          {item.state === "saving" && (
            <Typography>
              Saving — not complete, even if all bytes arrived.
            </Typography>
          )}
          {item.nextAttemptAt && (
            <Typography>Retry waiting until {item.nextAttemptAt}</Typography>
          )}
          {item.errorCode && (
            <Alert severity="warning">Item error: {item.errorCode}</Alert>
          )}
          {item.state === "needs_verification" && (
            <Alert severity="warning">
              Verify the destination; automatic or blind download retry is
              unsafe.
            </Alert>
          )}
          <Button
            disabled={disabled || !canRemoveQueueItem(item)}
            onClick={() => {
              setSelected([]);
              void onRemove([item.id]);
            }}
            aria-label={`Remove item ${item.name}`}
          >
            Remove
          </Button>
          {item.state === "failed" && (
            <Button
              disabled={disabled}
              onClick={() => void onRetry(item.id)}
              aria-label={`Retry failed item ${item.name}`}
            >
              Retry failed item (new bounded attempt)
            </Button>
          )}
        </Box>
      ))}
    </Stack>
  );
}
