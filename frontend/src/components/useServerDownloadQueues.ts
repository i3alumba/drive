import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { AccessDeniedError } from "../api";
import {
  newSubmissionId,
  queueApi,
  QueueRequestError,
  readPendingSubmission,
  storePendingSubmission,
  type QueueItemPage,
  type QueuePage,
  type QueueSubmission,
  type QueueSummary,
} from "../serverDownloadQueues";

export function useServerDownloadQueues(
  enabled: boolean,
  accessDenied: boolean,
) {
  const [page, setPage] = useState<QueuePage | null>(null);
  const [offset, setOffset] = useState(0);
  const [selected, setSelected] = useState<string | null>(null);
  const [detail, setDetail] = useState<QueueSummary | null>(null);
  const [items, setItems] = useState<QueueItemPage | null>(null);
  const [itemOffset, setItemOffset] = useState(0);
  const [error, setError] = useState("");
  const [authStopped, setAuthStopped] = useState(false);
  const [busy, setBusy] = useState(false);
  const [pending, setPending] = useState(readPendingSubmission);
  const [draftReset, setDraftReset] = useState(0);
  const pendingBody = useRef<QueueSubmission | null>(null);
  const mutationLock = useRef(false);
  const operation = useRef(0);
  const epoch = useRef(0);
  const stopped = useRef(false);
  const polling = useRef(enabled);
  const denied = useRef(accessDenied);
  const readGeneration = useRef(0);
  polling.current = enabled;
  denied.current = accessDenied;
  const current = useCallback(
    (version: number) =>
      !stopped.current && !denied.current && epoch.current === version,
    [],
  );

  const stopAuth = useCallback(() => {
    if (stopped.current) return;
    stopped.current = true;
    epoch.current++;
    readGeneration.current++;
    operation.current++;
    mutationLock.current = false;
    pendingBody.current = null;
    setBusy(false);
    setAuthStopped(true);
    setPage(null);
    setSelected(null);
    setDetail(null);
    setItems(null);
    setOffset(0);
    setItemOffset(0);
    setDraftReset((value) => value + 1);
    setError(
      "Drive access is unavailable. Queue controls and polling stopped; accepted server work continues. Sign in and reload before managing queues.",
    );
    // Only the unresolved opaque nonce survives; no owner A source body survives.
  }, []);
  useLayoutEffect(() => {
    if (accessDenied) stopAuth();
  }, [accessDenied, stopAuth]);
  useEffect(
    () => () => {
      epoch.current++;
    },
    [],
  );

  const fail = useCallback(
    (cause: unknown) => {
      if (
        cause instanceof AccessDeniedError ||
        (cause instanceof Error && cause.message === "Authentication required")
      )
        stopAuth();
      else
        setError(
          cause instanceof QueueRequestError
            ? cause.message
            : "Server queue connection unavailable. Retry when connected.",
        );
    },
    [stopAuth],
  );

  const refresh = useCallback(async () => {
    const version = epoch.current;
    if (!polling.current || !current(version)) return;
    const generation = ++readGeneration.current;
    const valid = () =>
      current(version) &&
      polling.current &&
      generation === readGeneration.current;
    try {
      const nextPage = await queueApi.list(offset);
      if (!valid()) return;
      setPage(nextPage);
      if (selected) {
        const nextDetail = await queueApi.detail(selected);
        if (!valid()) return;
        const nextItems = await queueApi.items(selected, itemOffset);
        if (!valid()) return;
        setDetail(nextDetail);
        setItems(nextItems);
      }
    } catch (cause) {
      if (!current(version)) return;
      // A rejected selection must not leave cached details from an earlier read.
      if (generation === readGeneration.current && selected) {
        setDetail(null);
        setItems(null);
        setSelected(null);
      }
      fail(cause);
    }
  }, [offset, selected, itemOffset, current, fail]);

  const accept = useCallback(
    (receipt: QueueSummary, version: number) => {
      if (!current(version)) return false;
      // Invalidate any concurrent receipt callback before a new draft can exist.
      epoch.current++;
      readGeneration.current++;
      storePendingSubmission(null);
      setPending(null);
      pendingBody.current = null;
      setDraftReset((value) => value + 1);
      setSelected(receipt.id);
      setItemOffset(0);
      setDetail(receipt);
      setError("");
      return true;
    },
    [current],
  );

  const receipt = useCallback(
    async (id: string, version: number) => {
      try {
        const confirmed = await queueApi.receipt(id);
        if (!current(version)) return "stale";
        return accept(confirmed, version) ? "accepted" : "stale";
      } catch (cause) {
        if (!current(version)) return "stale";
        if (
          cause instanceof QueueRequestError &&
          [404, 410].includes(cause.status)
        ) {
          setError(
            cause.status === 404
              ? "Receipt not found yet. Check again, or retry the unchanged draft with the same submission ID. No source draft is stored across reload."
              : "Submission receipt expired (410). Do not blindly repeat the original intent; check server queues before explicitly forgetting local tracking.",
          );
          return cause.status === 404 ? "missing" : "expired";
        }
        throw cause;
      }
    },
    [accept, current],
  );

  useEffect(() => {
    const version = epoch.current;
    if (!enabled || !current(version) || !pending || pendingBody.current)
      return;
    void receipt(pending, version).catch((cause) => {
      if (current(version)) fail(cause);
    });
  }, [enabled, authStopped, pending, receipt, current, fail]);

  useEffect(() => {
    if (!enabled || authStopped) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      await refresh();
      if (!cancelled && current(epoch.current) && polling.current)
        timer = setTimeout(poll, 4000);
    };
    void poll();
    return () => {
      cancelled = true;
      clearTimeout(timer);
      readGeneration.current++;
    };
  }, [enabled, authStopped, refresh, current]);

  const run = async (work: (version: number) => Promise<void>) => {
    const version = epoch.current;
    if (!polling.current || !current(version) || mutationLock.current) return;
    const token = ++operation.current;
    mutationLock.current = true;
    setBusy(true);
    setError("");
    try {
      await work(version);
    } catch (cause) {
      if (current(version)) fail(cause);
    } finally {
      if (operation.current === token) {
        mutationLock.current = false;
        setBusy(false);
      }
    }
  };
  const reconcile = () =>
    run(async (version) => {
      if (pending) await receipt(pending, version);
    });
  const submit = (draft: Omit<QueueSubmission, "submissionId">) =>
    run(async (version) => {
      let body: QueueSubmission;
      if (pending) {
        const result = await receipt(pending, version);
        if (result !== "missing" || !current(version)) return;
        const original = pendingBody.current;
        if (
          !original ||
          JSON.stringify({ ...draft, submissionId: pending }) !==
            JSON.stringify(original)
        ) {
          setError(
            "Unresolved original submission: check its receipt before submitting a changed draft or destination. No duplicate was submitted.",
          );
          return;
        }
        body = original;
      } else {
        const previous = pendingBody.current;
        body =
          previous &&
          JSON.stringify({ ...draft, submissionId: previous.submissionId }) ===
            JSON.stringify(previous)
            ? previous
            : { ...draft, submissionId: newSubmissionId() };
        pendingBody.current = body;
        storePendingSubmission(body.submissionId);
        setPending(body.submissionId);
      }
      if (!current(version)) return;
      try {
        accept(await queueApi.submit(body), version);
      } catch (cause) {
        if (!current(version)) return;
        if (
          cause instanceof QueueRequestError &&
          cause.status < 500 &&
          ![408, 409, 410].includes(cause.status)
        ) {
          // Includes definitive pre-acceptance destination_collision (422).
          storePendingSubmission(null);
          setPending(null);
          throw cause;
        }
        fail(cause);
        if (current(version)) {
          try {
            await receipt(body.submissionId, version);
          } catch (receiptError) {
            if (current(version)) fail(receiptError);
          }
        }
      }
    });

  const control = (id: string, action: "pause" | "resume" | "cancel") =>
    run(async (version) => {
      await queueApi.control(id, action);
      if (current(version)) await refresh();
    });
  const itemControl = (
    id: string,
    itemId: string,
    action: "cancel" | "retry",
  ) =>
    run(async (version) => {
      if (action === "retry") await queueApi.retryItem(id, itemId);
      else await queueApi.cancelItem(id, itemId);
      if (current(version)) await refresh();
    });
  const forget = () => {
    epoch.current++;
    readGeneration.current++;
    operation.current++;
    mutationLock.current = false;
    pendingBody.current = null;
    storePendingSubmission(null);
    setPending(null);
    setBusy(false);
    setDraftReset((value) => value + 1);
    if (!stopped.current) setError("");
  };

  return {
    page,
    offset,
    setOffset,
    selected,
    detail,
    items,
    itemOffset,
    setItemOffset,
    select: (id: string) => {
      if (!current(epoch.current)) return;
      setSelected(id);
      setDetail(null);
      setItems(null);
      setItemOffset(0);
    },
    error,
    authStopped,
    busy,
    pending,
    draftReset,
    refresh,
    reconcile,
    submit,
    control,
    itemControl,
    forget,
  };
}
