//go:build headless

package main

import (
	"testing"

	"foxtrack-bridge/internal/testhome"
)

func TestMain(m *testing.M) { testhome.Main(m) }
