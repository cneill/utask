package gzip_test

import (
	"testing"

	"github.com/cneill/utask/pkg/compress/gzip"
	"github.com/cneill/utask/pkg/compress/tests"
)

func TestCompression(t *testing.T) {
	tests.CompressionTests(t, gzip.New())
}
