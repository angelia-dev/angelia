//! crates.io source for Angelia, as an Extism WASM plugin (prototype for
//! angelia-dev/angelia#59, benchmark 4).
//!
//! All HTTP goes through the host's `http_request` (netguard, rate limits,
//! `allowed_hosts`). It shows the three patterns of architecture.md §10 Q1:
//!   - auth headers: an optional `token` plugin config value is sent as
//!     `Authorization`;
//!   - ETags: the first page is a conditional request built from the fetch
//!     state; a 304 ends the call with `not_modified`;
//!   - pagination: `meta.next_page` is followed, up to `MAX_PAGES` pages.
//!
//! Plugin config (Extism config map):
//!   - `base_url`: default `https://crates.io`;
//!   - `token`: optional crates.io API token.

use std::collections::BTreeMap;

use angelia_plugin_abi::{
    self as abi, FetchState, HttpRequest, ListInput, ListOutput, Target, ValidateOutput, Validators,
    Version,
};
use extism_pdk::{Error, FnResult, Json, config, plugin_fn};
use serde::Deserialize;

const KIND: &str = "crates";
const DEFAULT_BASE_URL: &str = "https://crates.io";
const MAX_PAGES: usize = 50;

#[derive(Deserialize)]
struct VersionsPage {
    versions: Vec<CrateVersion>,
    #[serde(default)]
    meta: Meta,
}

#[derive(Deserialize, Default)]
struct Meta {
    #[serde(default)]
    next_page: Option<String>,
}

#[derive(Deserialize)]
struct CrateVersion {
    num: String,
    #[serde(default)]
    created_at: String,
    #[serde(default)]
    yanked: bool,
    #[serde(default)]
    checksum: Option<String>,
    #[serde(default)]
    license: Option<String>,
    #[serde(default)]
    rust_version: Option<String>,
    #[serde(default)]
    dl_path: Option<String>,
}

/// Checks a crate name the way crates.io does: ASCII letters, digits, `-`
/// and `_`, starting with a letter, at most 64 characters.
fn check_name(t: &Target) -> Result<&str, String> {
    if t.kind != KIND {
        return Err(format!("kind {:?} is not {KIND:?}", t.kind));
    }
    let name = t.coordinates.get("name").map(String::as_str).unwrap_or("");
    if name.is_empty() {
        return Err("coordinate \"name\" is required".into());
    }
    if let Some(k) = t.coordinates.keys().find(|k| k.as_str() != "name") {
        return Err(format!("unknown coordinate {k:?}"));
    }
    if name.len() > 64 {
        return Err("crate name is longer than 64 characters".into());
    }
    let mut chars = name.chars();
    if !chars.next().is_some_and(|c| c.is_ascii_alphabetic()) {
        return Err("crate name must start with an ASCII letter".into());
    }
    if !chars.all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_') {
        return Err("crate name may only hold ASCII letters, digits, '-' and '_'".into());
    }
    Ok(name)
}

#[plugin_fn]
pub fn validate(Json(t): Json<Target>) -> FnResult<Json<ValidateOutput>> {
    Ok(Json(ValidateOutput { reason: check_name(&t).err().unwrap_or_default() }))
}

#[plugin_fn]
pub fn list(Json(input): Json<ListInput>) -> FnResult<Json<ListOutput>> {
    let name = check_name(&input.target).map_err(Error::msg)?;
    let base = config::get("base_url")?.unwrap_or_else(|| DEFAULT_BASE_URL.to_string());
    let base = base.trim_end_matches('/');
    let first = format!("{base}/api/v1/crates/{name}/versions");

    let mut headers = BTreeMap::new();
    headers.insert("Accept".to_string(), "application/json".to_string());
    if let Some(token) = config::get("token")? {
        headers.insert("Authorization".to_string(), token);
    }

    let mut versions = Vec::new();
    let mut state = FetchState::default();
    let mut url = first.clone();
    for page in 0..MAX_PAGES {
        // Only the first page is conditional: if it is unchanged, so is the list.
        let validators = (page == 0).then(|| Validators::from(&input.state));
        let resp = abi::http(HttpRequest {
            url: url.clone(),
            headers: headers.clone(),
            validators,
            ..Default::default()
        })?;
        if page == 0 {
            if resp.not_modified {
                let v = resp.validators;
                return Ok(Json(ListOutput {
                    state: FetchState { etag: v.etag, last_modified: v.last_modified, cursor: String::new() },
                    not_modified: true,
                    ..Default::default()
                }));
            }
            state.etag = resp.validators.etag.clone();
            state.last_modified = resp.validators.last_modified.clone();
        }
        match resp.status {
            200 => {}
            404 => return Err(Error::msg(format!("crate {name:?} not found")).into()),
            s => return Err(Error::msg(format!("{url}: unexpected status {s}")).into()),
        }
        let body: VersionsPage = serde_json::from_slice(&resp.body_bytes()?)?;
        versions.extend(body.versions.into_iter().map(|v| to_version(base, name, v)));
        match body.meta.next_page {
            // next_page is a query string relative to the first page's URL.
            Some(next) if next.starts_with('?') => url = format!("{first}{next}"),
            Some(next) if !next.is_empty() => {
                return Err(Error::msg(format!("unexpected next_page {next:?}")).into());
            }
            _ => return Ok(Json(ListOutput { versions, state, not_modified: false })),
        }
        abi::log("debug", format!("crates {name}: following page {}", page + 2));
    }
    Err(Error::msg(format!("crate {name:?}: more than {MAX_PAGES} pages")).into())
}

fn to_version(base: &str, name: &str, v: CrateVersion) -> Version {
    let mut metadata = BTreeMap::new();
    let mut put = |k: &str, v: Option<String>| {
        if let Some(v) = v {
            metadata.insert(k.to_string(), serde_json::Value::String(v));
        }
    };
    put("checksum", v.checksum);
    put("license", v.license);
    put("rust_version", v.rust_version);
    put("download_url", v.dl_path.map(|p| format!("{base}{p}")));
    Version {
        url: format!("https://crates.io/crates/{name}/{}", v.num),
        raw: v.num,
        published_at: v.created_at,
        prerelease: None, // crates.io has no upstream flag; the core derives it from SemVer
        yanked: v.yanked,
        notes_url: String::new(),
        metadata,
    }
}
