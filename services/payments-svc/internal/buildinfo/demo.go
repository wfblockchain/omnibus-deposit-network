//go:build demo

package buildinfo

// Demo is true in demo builds (-tags demo): static bearer tokens and the
// scenario clock are allowed.
const Demo = true
