package init

import (
	"github.com/cneill/utask/pkg/compress"
	"github.com/cneill/utask/pkg/compress/gzip"
	"github.com/cneill/utask/pkg/compress/noop"
)

// Register registers default compression algorithms.
func Register() error {
	noopCompress := noop.New()

	for name, c := range map[string]compress.Compression{
		"":                 noopCompress, // to ensure backwards compatibility
		noop.AlgorithmName: noopCompress,
		gzip.AlgorithmName: gzip.New(),
	} {
		if err := compress.RegisterAlgorithm(name, c); err != nil {
			return err
		}
	}

	return nil
}
