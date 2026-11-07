# Chat and push sink (shoutrrr)

The `shoutrrr` sink posts a short plain-text message to a chat or push service when Angelia sees a
release. It sends through the service packages of the [shoutrrr](https://github.com/nicholas-fedor/shoutrrr)
library (v0.23.0). Decisions: [ADR 0026](adr/0026-shoutrrr-sink-whole-url-secret.md). Configuration keys:
[watch-file.md §2.4](watch-file.md#24-sinks). The contract is `ShoutrrrSinkConfig` in the
[API specification](https://github.com/angelia-dev/spec/blob/main/docs/api.md).

## Setup

1. Write the service URL in the [shoutrrr format](https://nicholas-fedor.github.io/shoutrrr/) of your
   service (table below). The **whole URL is a secret**: tokens sit in its userinfo, host or path.
2. Give it to Angelia without putting it in a file. Either store it in the instance, encrypted at rest,
   bound to the hosts the service sends to (`url_ref`, see [Stored secrets](configuration.md#stored-secrets)),
   or set it in an environment variable of the `angelia` process (`url_env`). Exactly one of the two.
   A sink created through the API may name only a variable listed in [`api.allowed_env`](configuration.md).

   ```
   angelia secret set slack-releases --from-file slack-url.txt --host hooks.slack.com
   ```

3. Declare the sink and a subscription:

   ```yaml
   sinks:
     - name: releases-chat
       kind: shoutrrr
       config:
         url_ref: slack-releases
         service: slack        # optional: the URL must be for this service; shows it without the secret
         # timeout: 10s
   subscriptions:
     - name: chat-all
       sink: releases-chat
   ```

4. Run `angelia config check --watches watches.yaml`. It reads each URL as a delivery would, sends nothing,
   and warns about a stored secret without `--host`, about a plain-`http` server and about a URL a delivery
   would refuse. It never prints the URL.

## Services

| Scheme | URL example | Where it sends (bind the secret to) |
|---|---|---|
| `slack` | `slack://hook:T000-B000-XXXX@webhook` (webhook) or `slack://xoxb:TOKEN@C0123456789` (bot) | `hooks.slack.com`, or `slack.com` for an `xoxb`/`xoxp` token |
| `discord` | `discord://TOKEN@WEBHOOK_ID` | `discord.com` |
| `telegram` | `telegram://BOT_TOKEN@telegram?chats=@channel,12345` | `api.telegram.org`, or the host of `apiurl=` |
| `ntfy` | `ntfy://[user:pass@]ntfy.example.com/topic` | the URL host (`ntfy.sh` when it is empty) |
| `gotify` | `gotify://gotify.example.com/APP_TOKEN` | the URL host |
| `matrix` | `matrix://:ACCESS_TOKEN@matrix.example.com?rooms=!room:matrix.example.com` | the URL host |

Every other scheme is refused, among them `generic` (use the [webhook sink](webhooks.md)), `smtp`, `xmpp` and
`mqtt`. So are `skiptlsverify=yes` (TLS is always verified; add a private CA with [`http.ca_file`](configuration.md)) and a
matrix URL with a user name (only the access-token form is allowed: a password login would run, and send the
password, on every delivery). Title, priority, tags and similar options come from the URL's query.

## What is checked at every delivery

The configuration only names the secret, so the destination is checked when the URL is read, on every attempt:

- the dispatcher derives the HTTP origins the service sends to (table above);
- each must be `https`, or `http` (`disabletls=yes`) only for a host in
  [`http.allow_http_hosts`](configuration.md); the service's credentials then travel in clear text;
- each must pass the SSRF guard (`http.allow_networks`);
- when the stored secret has hosts, each origin must be one of them. A secret without hosts can point at any
  public host, so bind it.

Every request the library makes is pinned to those origins and follows no redirect. A failure of these checks
happens before anything is sent and is **retried**: fix it by rewriting the secret, and the next attempt uses the
new value. `last_error` starts with `shoutrrr:` and names the reference and the origin (and the service when
the URL itself is refused), never the URL.

## Messages

Without a subscription `template`, the message is plain text per event type, for example:

```
requests 2.33.0 released (minor), previous 2.32.5
https://github.com/psf/requests/releases/tag/v2.33.0
```

The defaults are listed in [templates.md](templates.md#default-chat-messages). With a template, its output
is sent as the message text.

## Responses and retries

| Outcome | Result |
|---|---|
| `2xx` from every request | delivered |
| `3xx`, `4xx` other than `408` and `429` | dead at once (`HTTP 404 Not Found`, `redirect not followed`) |
| `408`, `429` (honouring `Retry-After`), `5xx` | retried |
| no response (`connection refused`, `timeout`, `TLS handshake failed`, ...) | retried |
| the service reports a failure after a `2xx` (telegram `ok:false`) | retried (`service error`) |
| a request to a host other than the derived origins | dead: this Angelia version does not support the service's new destination |

A send to several targets (telegram chats, matrix rooms, a long discord message in chunks) is not atomic: a
retry after a partial failure sends to every target again. Use one target per sink when duplicates matter.
