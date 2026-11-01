//! A misbehaving source plugin for the WASM host tests. The coordinate `mode`
//! picks the misbehaviour; `url` is the target of the network modes.
//! Never deploy it.

use std::collections::BTreeMap;

use angelia_plugin_abi::{self as abi, HttpRequest, ListInput, ListOutput, Target, ValidateOutput, Version};
use extism_pdk::{Error, FnResult, Json, plugin_fn};

fn coord<'a>(t: &'a Target, k: &str) -> &'a str {
    t.coordinates.get(k).map(String::as_str).unwrap_or("")
}

fn one(raw: &str) -> ListOutput {
    ListOutput {
        versions: vec![Version { raw: raw.to_string(), ..Default::default() }],
        ..Default::default()
    }
}

#[plugin_fn]
pub fn validate(Json(t): Json<Target>) -> FnResult<Json<ValidateOutput>> {
    match coord(&t, "mode") {
        "invalid" => Ok(Json(ValidateOutput { reason: "mode is invalid".into() })),
        "validate_fetch" => {
            abi::http(HttpRequest { url: coord(&t, "url").into(), ..Default::default() })?;
            Ok(Json(ValidateOutput::default()))
        }
        "validate_trap" => panic!("validate traps"),
        _ => Ok(Json(ValidateOutput::default())),
    }
}

#[plugin_fn]
pub fn list(Json(input): Json<ListInput>) -> FnResult<Json<ListOutput>> {
    let t = &input.target;
    let url = coord(t, "url").to_string();
    match coord(t, "mode") {
        // Extism's built-in HTTP, which bypasses netguard: must be denied.
        "builtin_http" => {
            let req = extism_pdk::HttpRequest::new(&url);
            let resp = extism_pdk::http::request::<()>(&req, None)?;
            Ok(Json(one(&format!("builtin-{}", resp.status_code()))))
        }
        // Our http_request; the status becomes the version.
        "fetch" => {
            let mut headers = BTreeMap::new();
            if let Some(a) = t.coordinates.get("authorization") {
                headers.insert("Authorization".to_string(), a.clone());
            }
            let method = coord(t, "method").to_string();
            let body = abi::encode_body(coord(t, "body").as_bytes());
            let resp = abi::http(HttpRequest { url, method, headers, body, ..Default::default() })?;
            let mut out = one(&resp.status.to_string());
            let body = String::from_utf8_lossy(&resp.body_bytes()?).into_owned();
            out.versions[0].metadata.insert("body".into(), body.into());
            Ok(Json(out))
        }
        // A plugin that swallows the host's error: the call must still fail.
        "fetch_ignore_error" => {
            let _ = abi::http(HttpRequest { url, ..Default::default() });
            Ok(Json(one("1.0.0")))
        }
        // The host's raw answer, error included, returned as metadata.
        "fetch_raw" => {
            let resp = abi::http_raw(HttpRequest {
                url,
                method: coord(t, "method").to_string(),
                body: coord(t, "raw_body").to_string(),
                ..Default::default()
            })?;
            let mut out = one("1.0.0");
            let e = resp.error.unwrap_or_default();
            out.versions[0].metadata.insert("code".into(), e.code.into());
            Ok(Json(out))
        }
        "many_requests" => {
            for _ in 0..1000 {
                abi::http(HttpRequest { url: url.clone(), ..Default::default() })?;
            }
            Ok(Json(one("1.0.0")))
        }
        "log" => {
            for level in ["debug", "info", "warn", "error", "bogus"] {
                abi::log(level, format!("hello at {level}"));
            }
            abi::log("info", "x".repeat(10_000));
            Ok(Json(one("1.0.0")))
        }
        "loop" => {
            let mut n: u64 = 0;
            loop {
                n = std::hint::black_box(n.wrapping_add(1));
            }
        }
        "trap" => panic!("list traps"),
        "error" => Err(Error::msg("plugin says no").into()),
        "alloc" => {
            let mut v: Vec<Vec<u8>> = Vec::new();
            loop {
                v.push(vec![1u8; 1 << 20]);
                std::hint::black_box(&v);
            }
        }
        "bad_published_at" => {
            let mut out = one("1.0.0");
            out.versions[0].published_at = "yesterday".into();
            Ok(Json(out))
        }
        "empty_raw" => Ok(Json(one(""))),
        _ => Ok(Json(one("1.0.0"))),
    }
}
