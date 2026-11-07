package githubdispatch

import "syscall"

var errConnRefused error = syscall.ECONNREFUSED
