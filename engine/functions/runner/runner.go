package functionrunner

import (
	"github.com/cneill/utask/engine/functions"
	"github.com/cneill/utask/engine/step"
)

// Init registers all the functions loaded as step.Runners.
func Init() error {
	for _, functionName := range functions.List() {
		function, _ := functions.Get(functionName)
		if err := step.RegisterRunner(functionName, function); err != nil {
			return err
		}
	}
	return nil
}
