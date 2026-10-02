// Package k6 holds the k6 workload script that autobench drives.
//
// The script is embedded into the binary so that `autobench` is a single
// self-contained executable; the controller writes it to a temp file at run
// time and hands it to the k6 binary.
package k6

import _ "embed"

// RunnerJS is the constant-arrival-rate k6 workload driven by AUTOBENCH_* env vars.
//
//go:embed runner.js
var RunnerJS string
