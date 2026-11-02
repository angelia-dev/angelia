// Command angelia-plugin-crates is the crates.io source as a go-plugin
// plugin binary (prototype, angelia-dev/angelia#60). The core starts it;
// run by hand, it refuses to serve (no handshake cookie).
package main

import "github.com/angelia-dev/angelia/bench/plugins/goplugin/crates"

func main() { crates.Serve() }
