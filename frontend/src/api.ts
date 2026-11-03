const loginUrl = "https://main.i3alumba.ru/login";

function getCookie(name: string) {
  const prefix = `${name}=`;
  return document.cookie
    .split(";")
    .map((value) => value.trim())
    .find((value) => value.startsWith(prefix))
    ?.slice(prefix.length);
}

export class AccessDeniedError extends Error {
  constructor() {
    super("Access to Drive is not granted to this account.");
  }
}

export function apiUpload(
  input: string,
  data: FormData,
  signal: AbortSignal,
  progress: (fraction: number | null) => void,
) {
  return new Promise<void>((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", input);
    const token = getCookie("access_token");
    if (token)
      xhr.setRequestHeader(
        "Authorization",
        `Bearer ${decodeURIComponent(token)}`,
      );
    const abort = () => xhr.abort();
    signal.addEventListener("abort", abort, { once: true });
    const finish = (error?: Error) => {
      signal.removeEventListener("abort", abort);
      if (error) reject(error);
      else resolve();
    };
    xhr.upload.onprogress = (event) =>
      progress(
        event.lengthComputable && event.total > 0
          ? event.loaded / event.total
          : null,
      );
    xhr.upload.onload = () => progress(1);
    xhr.onerror = () =>
      finish(new Error("Upload interrupted. Check your connection and retry."));
    xhr.onabort = () =>
      finish(new DOMException("Upload cancelled", "AbortError"));
    xhr.onload = () => {
      if (xhr.status === 401) {
        window.location.href = loginUrl;
        finish(new Error("Authentication required"));
      } else if (xhr.status === 403) finish(new AccessDeniedError());
      else if (xhr.status < 200 || xhr.status >= 300)
        finish(new Error(`Upload failed (HTTP ${xhr.status}).`));
      else finish();
    };
    if (signal.aborted) {
      finish(new DOMException("Upload cancelled", "AbortError"));
      return;
    }
    xhr.send(data);
  });
}

export async function apiFetch(input: string, init: RequestInit = {}) {
  const token = getCookie("access_token");
  const headers = new Headers(init.headers);
  if (token) {
    headers.set("Authorization", `Bearer ${decodeURIComponent(token)}`);
  }
  const response = await fetch(input, { ...init, headers });
  if (response.status === 401) {
    window.location.href = loginUrl;
    throw new Error("Authentication required");
  }
  if (response.status === 403) throw new AccessDeniedError();
  return response;
}
