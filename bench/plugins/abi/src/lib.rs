//! Draft Angelia source-plugin ABI, version 0 (prototype for angelia-dev/angelia#59).
//!
//! It mirrors `internal/plugin/wasm/wire.go` on the host. Everything here is a
//! draft and is not a public contract yet.
//!
//! A source plugin exports two Extism functions:
//!   - `validate`: [`ValidateInput`] -> [`ValidateOutput`]; no network access.
//!   - `list`: [`ListInput`] -> [`ListOutput`].
//!
//! It may import these host functions from the `angelia:host/v0` module:
//!   - `http_request`: [`HttpRequest`] -> [`HttpResponse`], sent through the
//!     core's netguard client (SSRF guard, per-host rate limit, retries) and
//!     limited to the manifest's `allowed_hosts`;
//!   - `log`: [`LogEntry`], written to the core's structured log.
//!
//! Extism's own `extism:host/env::http_request` is denied for every host.

use std::collections::BTreeMap;

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use extism_pdk::{Error, Json, host_fn};
use serde::{Deserialize, Serialize};

/// A target, as in `source.Target`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Target {
    pub kind: String,
    #[serde(default)]
    pub coordinates: BTreeMap<String, String>,
}

/// The opaque fetch state of a target, as in `source.FetchState`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct FetchState {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub etag: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_modified: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub cursor: String,
}

pub type ValidateInput = Target;

/// Output of `validate`: an empty reason means the target is valid.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ValidateOutput {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reason: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ListInput {
    pub target: Target,
    #[serde(default)]
    pub state: FetchState,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ListOutput {
    #[serde(default)]
    pub versions: Vec<Version>,
    #[serde(default)]
    pub state: FetchState,
    #[serde(default)]
    pub not_modified: bool,
}

/// One raw upstream version, as in `source.Version`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Version {
    pub raw: String,
    /// RFC 3339; empty if upstream does not say.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub published_at: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub prerelease: Option<bool>,
    #[serde(default)]
    pub yanked: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub url: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub notes_url: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub metadata: BTreeMap<String, serde_json::Value>,
}

/// Cache validators. When set on a request, the host sends them as
/// `If-None-Match` / `If-Modified-Since`; a response always carries the
/// validators to store (on a 304: the request's, updated by the response).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Validators {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub etag: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_modified: String,
}

impl From<&FetchState> for Validators {
    fn from(s: &FetchState) -> Self {
        Validators { etag: s.etag.clone(), last_modified: s.last_modified.clone() }
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct HttpRequest {
    /// GET (default), HEAD or POST.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub method: String,
    pub url: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub headers: BTreeMap<String, String>,
    /// Base64 (standard alphabet) request body.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub body: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub validators: Option<Validators>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct HttpError {
    pub code: String,
    pub message: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct HttpResponse {
    #[serde(default)]
    pub status: u16,
    /// Lower-case header names.
    #[serde(default)]
    pub headers: BTreeMap<String, Vec<String>>,
    /// Base64 (standard alphabet) response body; empty on a 304.
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub validators: Validators,
    #[serde(default)]
    pub not_modified: bool,
    /// Set when no response was received (denied host, SSRF block, transport error).
    #[serde(default)]
    pub error: Option<HttpError>,
}

impl HttpResponse {
    /// The decoded body.
    pub fn body_bytes(&self) -> Result<Vec<u8>, Error> {
        Ok(STANDARD.decode(&self.body)?)
    }

    /// The first value of a header, by lower-case name.
    pub fn header(&self, name: &str) -> Option<&str> {
        self.headers.get(name).and_then(|v| v.first()).map(String::as_str)
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct LogEntry {
    /// debug, info, warn or error.
    pub level: String,
    pub message: String,
}

mod imports {
    use super::*;

    #[host_fn("angelia:host/v0")]
    extern "ExtismHost" {
        pub fn http_request(req: Json<HttpRequest>) -> Json<HttpResponse>;
        pub fn log(entry: Json<LogEntry>);
    }
}

/// Sends req through the host. A request the host could not complete (denied
/// host, SSRF block, transport error, cancelled call) is an Err; any HTTP
/// status, 4xx and 5xx included, is an Ok response.
pub fn http(req: HttpRequest) -> Result<HttpResponse, Error> {
    let Json(resp) = unsafe { imports::http_request(Json(req))? };
    if let Some(e) = &resp.error {
        return Err(Error::msg(format!("http_request: {}: {}", e.code, e.message)));
    }
    Ok(resp)
}

/// Like [`http`] but returns the host's answer as is, error included.
pub fn http_raw(req: HttpRequest) -> Result<HttpResponse, Error> {
    let Json(resp) = unsafe { imports::http_request(Json(req))? };
    Ok(resp)
}

/// Writes one entry to the core's log.
pub fn log(level: &str, message: impl Into<String>) {
    let entry = LogEntry { level: level.to_string(), message: message.into() };
    let _ = unsafe { imports::log(Json(entry)) };
}

/// Encodes a request body.
pub fn encode_body(b: &[u8]) -> String {
    STANDARD.encode(b)
}
