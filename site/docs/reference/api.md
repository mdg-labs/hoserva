---
title: API reference
description: Every operation of Hoserva's HTTP API, with the role it needs and an example request, generated from the API specification.
---

Hoserva's web interface and command-line tool both work through one HTTP API, and anything they can do you can script. This reference lists every operation of the API, the schema of every request and response, and the live event stream. It is generated from the API specification that belongs to this version of the documentation, so it always matches the API it describes.

To get started, create a personal token and call one operation: [Script against the API with a token](./api-tokens.mdx).

## How do I call the API?

The API is served over HTTPS only, on port 8008 of your server, under the path `/api/v1`:

```text
https://nas.local:8008/api/v1
```

Every example in this reference reads the server name from `HOSERVA_HOST` and the token from `HOSERVA_TOKEN`, and replaces each `{placeholder}` in a path with a real value. By default, Hoserva uses a self-signed certificate that `curl` does not trust. Add `--insecure` to an example while you test on a network you control, or point `--cacert` at a certificate you trust.

Hoserva accepts connections only from private network addresses unless you change that on **Settings → Network**. See [Exposing services safely](../guides/exposing-safely.mdx) before you open it up.

## How does a request authenticate?

Send a personal API token in an `Authorization` header:

```text
Authorization: Bearer <your token>
```

The web interface signs in with a session cookie instead, and a script has no need for one. On the server itself, the command-line tool talks to Hoserva through a local socket that only root and the `hoserva` group can open, so it needs no token.

## What does the required role mean?

Each operation page names the role it needs. Accounts and tokens have one of two roles:

| Role | What it can call |
|---|---|
| Viewer | Operations that only read state |
| Admin | Every operation, including the ones that change settings, disks, shares and apps |

A few operations, such as signing in, need no credential. A token has its own role, set when you create it, and it can never be wider than the role of the account it belongs to. An account with only SMB or NFS access cannot have a token.

## How are errors reported?

A failed request returns a non-success status and a JSON body with a stable `code` and a readable `message`. An unknown, revoked or missing token returns `401` with the code `unauthorized`. A valid token whose role is too narrow for the operation returns `403` with the code `forbidden`.

## How do I follow live events?

[Stream live events](./api/stream-events.api.mdx) keeps a connection open and sends job progress, disk and container state changes, and notifications as they happen. The shape of each event is described by the [Event](./api/schemas/event.schema.mdx) schema and the event types it lists.

## Next steps

- [Script against the API with a token](./api-tokens.mdx)
- [Exposing services safely](../guides/exposing-safely.mdx)
