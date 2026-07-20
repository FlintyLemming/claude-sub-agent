package main

import (
	"errors"
	"testing"
)

// fixedTokens is a TokenProvider stub returning a preset blob or error.
type fixedTokens struct {
	cred []byte
	err  error
}

func (f *fixedTokens) Credentials() ([]byte, error) { return f.cred, f.err }

func TestChainTokens(t *testing.T) {
	errA := errors.New("file missing")
	errB := errors.New("keychain empty")

	t.Run("first provider succeeds, later ones not consulted", func(t *testing.T) {
		second := &fixedTokens{err: errB}
		chain := newChainTokens(&fixedTokens{cred: []byte("from-file")}, second)
		got, err := chain.Credentials()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "from-file" {
			t.Fatalf("got %q, want %q", got, "from-file")
		}
	})

	t.Run("falls back to second when first fails", func(t *testing.T) {
		chain := newChainTokens(&fixedTokens{err: errA}, &fixedTokens{cred: []byte("from-keychain")})
		got, err := chain.Credentials()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "from-keychain" {
			t.Fatalf("got %q, want %q", got, "from-keychain")
		}
	})

	t.Run("all fail, errors joined", func(t *testing.T) {
		chain := newChainTokens(&fixedTokens{err: errA}, &fixedTokens{err: errB})
		_, err := chain.Credentials()
		if err == nil {
			t.Fatal("expected error when all providers fail")
		}
		if !errors.Is(err, errA) || !errors.Is(err, errB) {
			t.Fatalf("joined error missing a cause: %v", err)
		}
	})
}
