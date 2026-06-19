const loginUrl = "https://main.i3alumba.ru/login";

function getCookie(name: string) {
  const prefix = `${name}=`;
  return document.cookie
    .split(";")
    .map((value) => value.trim())
    .find((value) => value.startsWith(prefix))
    ?.slice(prefix.length);
}

export async function apiFetch(input: string, init: RequestInit = {}) {
  const token = getCookie("access_token");
  const headers = new Headers(init.headers);
  if (token) {
    headers.set("Authorization", `Bearer ${decodeURIComponent(token)}`);
  }
  const response = await fetch(input, { ...init, headers });
  if (response.status === 401) window.location.href = loginUrl;
  return response;
}
