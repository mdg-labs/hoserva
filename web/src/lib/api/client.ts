// The only place the web UI calls the API from (D18, doc 12 §2): every
// other module imports hoservaClient from here rather than the generated
// client directly, so there is exactly one client instance and one place
// that knows the base URL. api/openapi.yaml's `servers` entries carry the
// `/api/v1` prefix, so paths.d.ts keys (e.g. `/jobs`) are relative to it.
import { createHoservaClient } from "@api/client";

const SESSION_SECRET_HEADER = "X-Hoserva-Session-Secret";
const SESSION_SECRET_KEY = "hoserva.sessionSecret";

// The secret also lives here, for a browser that blocks sessionStorage.
let memorySecret: string | null = null;

// A browser sends the session cookie to every service on this host name,
// whatever its port, so the cookie alone does not authenticate a request
// (doc 15 T15). Login returns a second secret that only this origin's script
// can read; it is kept in sessionStorage, which another port's origin cannot
// see, and goes out as a header on every request.
function storedSessionSecret(): string | null {
  try {
    return globalThis.sessionStorage?.getItem(SESSION_SECRET_KEY) ?? null;
  } catch {
    return null;
  }
}

function storeSessionSecret(secret: string): void {
  memorySecret = secret;
  try {
    globalThis.sessionStorage?.setItem(SESSION_SECRET_KEY, secret);
  } catch {
    // The in-memory copy serves until the page is reloaded.
  }
}

function clearSessionSecret(): void {
  memorySecret = null;
  try {
    globalThis.sessionStorage?.removeItem(SESSION_SECRET_KEY);
  } catch {
    // Nothing was stored.
  }
}

export function sessionHeaders(): Record<string, string> {
  const secret = memorySecret ?? storedSessionSecret();
  return secret === null ? {} : { [SESSION_SECRET_HEADER]: secret };
}

export const hoservaClient = createHoservaClient("/api/v1");

hoservaClient.use({
  onRequest({ request }) {
    for (const [name, value] of Object.entries(sessionHeaders())) {
      request.headers.set(name, value);
    }
    return request;
  },
  onResponse({ response, schemaPath }) {
    if (!response.ok) {
      return undefined;
    }
    if (schemaPath === "/auth/login" || schemaPath === "/setup/admin") {
      const secret = response.headers.get(SESSION_SECRET_HEADER);
      if (secret) {
        storeSessionSecret(secret);
      }
    } else if (schemaPath === "/auth/logout") {
      clearSessionSecret();
    }
    return undefined;
  },
});

export type { components, operations, paths } from "@api/client";
