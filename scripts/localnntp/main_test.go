package main

import (
	"os"
	"testing"
)

// TestMain pins par2 to one OpenMP thread. The gen tests run par2 on kilobyte
// inputs and are t.Parallel, and par2cmdline's default of one spinning thread
// per core makes concurrent runs contend for ~16s where one takes ~30ms.
// Release generation outside the tests is unaffected.
func TestMain(m *testing.M) {
	if err := os.Setenv("OMP_NUM_THREADS", "1"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
