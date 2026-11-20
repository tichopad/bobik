// Package sandbox runs Bash in an offline Linux Bubblewrap sandbox with a
// writable live vault. Mount exceptions hide, make read-only, or pin vault
// paths. They do not constrain trusted host processes.
package sandbox
