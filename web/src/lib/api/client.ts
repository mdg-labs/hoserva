// The only place the web UI calls the API from (D18, doc 12 §2): every
// other module imports hoservaClient from here rather than the generated
// client directly, so there is exactly one client instance and one place
// that knows the base URL. api/openapi.yaml's `servers` entries carry the
// `/api/v1` prefix, so paths.d.ts keys (e.g. `/jobs`) are relative to it.
import { createHoservaClient } from "@api/client";

export const hoservaClient = createHoservaClient("/api/v1");

export type { components, operations, paths } from "@api/client";
