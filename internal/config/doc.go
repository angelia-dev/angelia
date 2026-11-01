// Package config loads and validates the instance configuration: the
// settings of one Angelia process (listen address, database, roles, logging).
// Watches and subscriptions are not part of it.
//
// Sources, lowest to highest precedence: built-in defaults, an optional YAML
// file, then ANGELIA_<SECTION>_<KEY> environment variables. See
// docs/configuration.md for the key table and defaults.
package config
