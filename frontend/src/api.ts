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
