// Package render turns a release event into the body a sink delivers, with
// the subscription's Go text/template or, when it has none, the event's
// CloudEvents JSON (architecture.md §3.5, ADR 0016, docs/templates.md).
//
// A template sees the event envelope as its JSON object: the dot is the
// decoded CloudEvents structured-mode event, so the fields have their spec
// names (.type, .id, .subject, .data.package, .data.watch.labels.team). A
// missing key is an error, not "<no value>".
//
// Templates are written by whoever controls the configuration, but they run
// inside the dispatcher, so execution is bounded:
//
//   - [Compile] rejects templates over [Limits.MaxTemplateBytes] and the
//     define, template and block actions, then instruments every range body
//     with a deadline check;
//   - execution stops with [ErrTimeout] after [Limits.Timeout] (or the
//     context's deadline, if earlier), and with [ErrOutputTooLarge] when the
//     output, or any string a function builds, would pass
//     [Limits.MaxOutputBytes];
//   - the functions are a curated set ([FuncNames]): no environment, file,
//     network or clock access, and the formatting builtins are replaced by
//     size-checked versions.
//
// [Template.Check] runs a template against the sample events ([Samples]),
// which is how configuration loading validates it.
package render
