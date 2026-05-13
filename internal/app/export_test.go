// export_test.go exposes internal state for white-box testing.
// This file is compiled only during test builds.
package app

import "context"

// ForceAssemblerStopped starts then stops the assembler so it is in a
// permanently-stopped state. A subsequent app.Start will fail at the
// assembler.Start step (assembler returns ErrStopped). Used to test that a
// failed Start resets started=false so the application can be retried.
func (a *Application) ForceAssemblerStopped() error {
	if err := a.assembler.Start(context.Background()); err != nil {
		return err
	}
	return a.assembler.Stop()
}
