# MQTT sink

The `mqtt` sink publishes each matching release event to an MQTT broker (Mosquitto, EMQX, HiveMQ, a home
automation hub). It speaks MQTT 3.1.1 and only publishes: it never subscribes.

Configuration keys: [watch-file.md §2.4](watch-file.md#24-sinks). The contract is `MqttSinkConfig` in the
[API specification](https://github.com/angelia-dev/spec/blob/main/docs/api.md) (decision 56). The security
rules are [ADR 0039](adr/0039-non-http-sinks-dial-through-netguard.md).

## Setup

1. Create a user for Angelia on the broker, allowed to publish to the topics you choose. With Mosquitto 2.x
   and TLS on port 8883:

   ```
   # mosquitto.conf
   listener 8883
   certfile /mosquitto/certs/server.crt
   keyfile  /mosquitto/certs/server.key
   password_file /mosquitto/config/passwd
   acl_file /mosquitto/config/acl

   # acl
   user angelia
   topic write angelia/#
   ```

   ```
   mosquitto_passwd -c /mosquitto/config/passwd angelia
   ```

2. Give the password to Angelia without putting it in a file: store it in the instance, encrypted at rest and
   bound to the broker (`password_ref`, see [Stored secrets](configuration.md#stored-secrets)), or put it in an
   environment variable of the `angelia` process (`password_env`). Bind a broker secret with an **explicit
   port**: a host without a port means 443.

   ```
   angelia secret set mqtt-broker --from-file - --host broker.example.net:8883
   ```

3. Declare the sink and a subscription:

```yaml
sinks:
  - name: broker
    kind: mqtt
    config:
      url: mqtts://broker.example.net:8883
      topic: 'angelia/{{.data.source}}/{{.data.package}}'
      username: angelia
      password_ref: mqtt-broker
      # qos: 1          # the default: delivered once the broker answers PUBACK
      # retain: false   # the default
subscriptions:
  - name: releases-to-broker
    sink: broker
```

If the broker's certificate comes from a private CA, add the CA to `http.ca_file`
([configuration.md](configuration.md#outgoing-http-and-private-networks)). If the broker is on a private
network, see [Private brokers](#private-brokers).

## Topics

`topic` is a [template](templates.md) with the context of a subscription template, checked against the sample
events when the file loads. Examples:

| `topic` | Topic for `requests` 2.33.0 from PyPI |
| --- | --- |
| `angelia/releases` | `angelia/releases` |
| `angelia/{{.data.source}}/{{.data.package}}` | `angelia/pypi/requests` |
| `releases/{{.data.watch.labels.team}}` | `releases/platform` (guard optional labels with `with`) |

The rendered topic must be 1 to 1024 bytes of valid UTF-8, without the wildcards `+` and `#` or U+0000, and
must not start with `$` (reserved by brokers, such as `$SYS`). A package name with `/` (a GitHub
`owner/repo`) adds topic levels. A topic that fails to render, or renders to an invalid topic, is a permanent
delivery error.

## Payload

The payload is the rendered message, as for a webhook: the **CloudEvents JSON** of the event by default
(structured mode, the only mode the CloudEvents MQTT binding allows for MQTT 3.1.1, which has no headers), or
the output of the subscription's `template`. A template replaces the whole payload, so a home automation hub
can receive just `{{.data.package}} {{.data.version}}`.

## Delivery semantics

Each attempt opens one connection: `CONNECT` (clean session, a fresh client id made of `client_id_prefix` and
random letters and digits, 23 characters in all), `PUBLISH`, `DISCONNECT`.

- **`qos: 1`** (the default): the attempt succeeds when the broker answers `PUBACK`. No `PUBACK` before the
  timeout is a failure, retried.
- **`qos: 0`**: the attempt succeeds once `PUBLISH` and `DISCONNECT` are written; the broker confirms
  nothing.
- QoS 2 is not offered: the dispatcher's retries can repeat a publish anyway, so it would buy nothing.
- **ACL caveat.** MQTT 3.1.1 has no reason code on `PUBACK`: a broker that denies a publish by ACL may still
  acknowledge it (Mosquitto does) or close the connection. A denied publish can therefore look delivered;
  check the broker's ACL and log when messages do not arrive.
- **At least once.** As for every sink, a `PUBACK` lost on the way back means the event is published again on
  the retry. Consumers deduplicate on the CloudEvents `id` in the payload (a template that replaces the
  payload should carry `{{.id}}` if duplicates matter).
- `retain: true` keeps the last message of the topic on the broker for new subscribers.

## Errors and retries

| Outcome | Class |
| --- | --- |
| `CONNACK` 1 (protocol version), 4 (bad user name or password), 5 (not authorized) | permanent |
| SSRF refusal, `mqtt://` to a host outside `http.allow_http_hosts`, invalid configuration, topic that fails | permanent |
| `CONNACK` 2 (identifier rejected), 3 (server unavailable) | retried |
| connection refused or reset, timeout, TLS handshake failure, no `CONNACK` or `PUBACK` in time | retried |
| a protocol error (a packet a 3.1.1 publisher does not expect) | retried |
| an unset or empty password variable, a stored secret not found | retried |

`last_error` reads `mqtt: mqtts://broker.example.net:8883: <class>`: never the user name, the password, the
topic or the payload. Retries follow the dispatcher's backoff; a dead delivery can be replayed.

## TLS and plaintext

- `mqtts://` (default port 8883) uses TLS 1.2 or later, verified against the system roots plus `http.ca_file`.
  Verification cannot be switched off. Client certificates are not supported yet.
- `mqtt://` (default port 1883) is cleartext. It is accepted only for a host listed in `http.allow_http_hosts`,
  checked when the file loads and again at every delivery. A password over `mqtt://` travels in clear, and
  the watch file warns.

## Private brokers

Every connection goes through the same SSRF guard as outgoing HTTP, on the address actually connected. A
broker on a private network (`192.168.1.10`, `mosquitto.lan`) therefore needs its prefix in
`http.allow_networks`, exactly like a webhook on a LAN, and a plain `mqtt://` broker also needs its host in
`http.allow_http_hosts`:

```
http:
  allow_networks: [192.168.1.0/24]
  allow_http_hosts: [mosquitto.lan]
```

`config check` warns when a broker URL is a private address literal outside `http.allow_networks`. A host
name that resolves to a private address fails each delivery with a permanent error naming the host. The
per-host rate limits of `http.host_limits` apply to the broker host.

## Not supported

- **No proxy.** The broker must be reachable directly; `HTTPS_PROXY` does not apply to MQTT.
- **No WebSockets** (`ws://`, `wss://`, `mqtt+ws://`).
- MQTT 5, QoS 2, persistent sessions and client certificates.

## Live test

`internal/sink/mqtt/live_test.go` runs against a real broker behind the `live` build tag. It is not run in CI:

```
docker run --rm -p 1883:1883 eclipse-mosquitto:2 mosquitto -c /mosquitto-no-auth.conf
ANGELIA_LIVE_MQTT_URL=mqtt://localhost:1883 go test -tags live -run Live ./internal/sink/mqtt/
```

Set `ANGELIA_LIVE_MQTT_USERNAME` and `ANGELIA_LIVE_MQTT_PASSWORD` for a broker with a password, and
`ANGELIA_LIVE_MQTT_CA_FILE` for an `mqtts://` broker with a private CA.
