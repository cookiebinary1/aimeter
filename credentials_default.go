//go:build !omp

package main

// ompFallback is a no-op in default builds; build with -tags omp to read the
// local OMP agent database instead.
func ompFallback(*Creds, map[string]string) {}
