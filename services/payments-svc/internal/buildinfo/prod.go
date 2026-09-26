//go:build !demo

// Package buildinfo tells production builds from demo builds. Demo-only
// behaviour is gated on Demo, set by the "demo" build tag, never by
// configuration.
package buildinfo

// Demo is false in production builds.
const Demo = false
