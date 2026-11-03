// Package render turns a release event into the body a sink delivers, with
// the subscription's Go text/template or, when it has none, the event's
// CloudEvents JSON (architecture.md §3.5, ADR 0016, docs/templates.md).
//
// A template sees the event envelope as its JSON object: the dot is the
// decoded CloudEvents structured-mode event, so the fields have their spec
// names (.type, .id, .subject, .data.package). A missing key is an error,
// not "<no value>"; labels, which a watch may not have, are read with
// index: {{index .data.watch.labels "team" | default "none"}}.
//
// Templates are written by whoever controls the configuration, but they run
// inside the dispatcher, so execution is bounded:
//
//   - [Compile] rejects templates over [Limits.MaxTemplateBytes] and the
//     define, template and block actions, then instruments every range body
//     with a deadline check;
//   - execution stops with [ErrTimeout] after [Limits.Timeout] (or when the
//     context ends, if earlier), checked in every loop iteration, every
//     output write and every function call, and with [ErrOutputTooLarge]
//     when the output, or any string a function builds, would pass
//     [Limits.MaxOutputBytes];
//   - the functions are a curated set ([FuncNames]): no environment, file,
//     network or clock access; the formatting builtins are replaced by
//     size-checked versions that take scalar arguments only.
//
// Error messages ([Error]) give a position and a reason from a fixed set,
// never the template text.
//
// [Template.Check] runs a template against the sample events ([Samples]),
// which is how configuration loading validates it.
package render
